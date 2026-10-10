// Package cli implements the `ports` command-line interface.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/cmerk2021/ports/internal/app"
	"github.com/cmerk2021/ports/internal/audit"
	"github.com/cmerk2021/ports/internal/config"
	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "dev"

// Exit codes.
const (
	ExitOK           = 0
	ExitError        = 1 // general failure: I/O, corrupt files, missing confirmation
	ExitUsage        = 2 // invalid arguments
	ExitConflict     = 3 // allocation conflict, exhausted range, audit violations
	ExitAuditConfig  = 4 // audit could not complete: invalid Compose, Docker unavailable
	ExitNotFound     = 5 // stack, assignment or port not found
	ExitInconsistent = 6 // registry/manifest discrepancy requires `ports sync`
)

// usageError marks CLI argument errors.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

// env carries per-invocation state.
type env struct {
	ctx    context.Context
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	configPath, stackRoot, registryPath string
	jsonOut, yes                        bool

	newApp func(*config.Config) *app.App
}

func (e *env) addCommon(fs *flag.FlagSet) {
	fs.StringVar(&e.configPath, "config", e.configPath, "configuration file (default "+config.DefaultConfigPath+", env "+config.EnvConfig+")")
	fs.StringVar(&e.stackRoot, "stack-root", e.stackRoot, "stack root directory (env "+config.EnvStackRoot+")")
	fs.StringVar(&e.registryPath, "registry", e.registryPath, "global registry file (env "+config.EnvRegistry+")")
	fs.BoolVar(&e.yes, "yes", e.yes, "confirm destructive operations")
	fs.BoolVar(&e.jsonOut, "json", e.jsonOut, "machine-readable JSON output")
}

func (e *env) app() (*app.App, error) {
	cfg, err := config.Load(config.Overrides{ConfigPath: e.configPath, StackRoot: e.stackRoot, RegistryPath: e.registryPath})
	if err != nil {
		return nil, err
	}
	a := e.newApp(cfg)
	a.Confirm = e.confirm
	return a, nil
}

func (e *env) confirm(prompt string) error {
	if e.yes {
		return nil
	}
	if f, ok := e.stdin.(*os.File); !ok || !isTerminal(f) {
		return fmt.Errorf("%w: %s\nre-run with --yes to confirm non-interactively", app.ErrDeclined, prompt)
	}
	fmt.Fprintf(e.stderr, "%s\nType 'yes' to continue: ", prompt)
	line, _ := bufio.NewReader(e.stdin).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return fmt.Errorf("%w: aborted, nothing was changed", app.ErrDeclined)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Run executes the CLI and returns the process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(ctx, args, stdin, stdout, stderr, app.New)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, newApp func(*config.Config) *app.App) int {
	e := &env{ctx: ctx, stdin: stdin, stdout: stdout, stderr: stderr, newApp: newApp}
	global := flag.NewFlagSet("ports", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	e.addCommon(global)
	help := global.Bool("help", false, "")
	global.BoolVar(help, "h", false, "")
	if err := global.Parse(args); err != nil {
		return e.fail(usagef("%v", err))
	}
	rest := global.Args()
	if *help || len(rest) == 0 {
		fmt.Fprint(stdout, mainUsage)
		if len(rest) == 0 && !*help {
			return ExitUsage
		}
		return ExitOK
	}
	cmd, cmdArgs := rest[0], rest[1:]
	c, ok := commands[cmd]
	if !ok {
		if cmd == "help" {
			if len(cmdArgs) > 0 {
				if hc, ok := commands[cmdArgs[0]]; ok {
					fmt.Fprint(stdout, hc.usage)
					return ExitOK
				}
			}
			fmt.Fprint(stdout, mainUsage)
			return ExitOK
		}
		if cmd == "version" || cmd == "--version" {
			fmt.Fprintf(stdout, "ports %s\n", Version)
			return ExitOK
		}
		if strings.HasPrefix(cmd, "-") || model.ValidateStackName(cmd) != nil {
			if _, err := strconv.Atoi(cmd); err == nil {
				return e.fail(usagef("unknown command %q (to find a port use `ports find %s`)", cmd, cmd))
			}
			return e.fail(usagef("unknown command or invalid stack name %q (see `ports --help`)", cmd))
		}
		// `ports <stack>` is shorthand for `ports show <stack>`.
		c, cmdArgs = commands["show"], rest
	}
	fs := flag.NewFlagSet("ports "+c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	e.addCommon(fs)
	h := fs.Bool("help", false, "")
	fs.BoolVar(h, "h", false, "")
	run := c.setup(fs, e)
	pos, err := parseInterspersed(fs, cmdArgs)
	if *h {
		fmt.Fprint(stdout, c.usage)
		return ExitOK
	}
	if err != nil {
		return e.fail(usagef("%v\n\n%s", err, c.usage))
	}
	return e.finish(run(pos))
}

// parseInterspersed allows flags after positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		dashdash := -1
		for i, a := range args {
			if a == "--" {
				dashdash = i
				break
			}
		}
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if dashdash >= 0 && len(rest) == len(args)-dashdash-1 {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (e *env) finish(code int, err error) int {
	if err != nil {
		return e.fail(err)
	}
	return code
}

func exitCode(err error) int {
	var ue *usageError
	switch {
	case errors.As(err, &ue), errors.Is(err, app.ErrUsage):
		return ExitUsage
	case errors.Is(err, app.ErrConflict):
		return ExitConflict
	case errors.Is(err, app.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, app.ErrInconsistent):
		return ExitInconsistent
	default:
		return ExitError
	}
}

func (e *env) fail(err error) int {
	code := exitCode(err)
	msg := err.Error()
	if errors.Is(err, store.ErrRegistryNotFound) && !strings.Contains(msg, "ports init") {
		msg += "\nhint: initialize a new server with `ports init` (see `ports help init`)"
	}
	if e.jsonOut {
		_ = writeJSON(e.stdout, map[string]any{"error": msg, "exit_code": code})
	}
	fmt.Fprintf(e.stderr, "ports: %s\n", msg)
	return code
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (e *env) emitJSON(v any) (int, error) {
	if err := writeJSON(e.stdout, v); err != nil {
		return ExitError, err
	}
	return ExitOK, nil
}

// auditExit maps an audit report to an exit code.
func auditExit(r *audit.Report) int {
	switch {
	case r.Summary.Violations > 0:
		return ExitConflict
	case r.Summary.Errors > 0:
		return ExitAuditConfig
	default:
		return ExitOK
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func needArgs(pos []string, n int, usage string) error {
	if len(pos) != n {
		return usagef("expected %d argument(s), got %d\n\n%s", n, len(pos), usage)
	}
	return nil
}

func parsePortArg(what, s string) (int, error) {
	p, err := model.ParsePort(s)
	if err != nil {
		return 0, usagef("%s: %v", what, err)
	}
	return p, nil
}
