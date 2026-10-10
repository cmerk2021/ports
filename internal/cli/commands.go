package cli

import (
	"flag"
	"fmt"
	"strings"

	"github.com/cmerk2021/ports/internal/app"
	"github.com/cmerk2021/ports/internal/model"
)

type command struct {
	name  string
	usage string
	setup func(fs *flag.FlagSet, e *env) func(pos []string) (int, error)
}

var commands = map[string]*command{}

func register(c *command) { commands[c.name] = c }

const mainUsage = `ports - homelab host-port allocation registry for Docker Compose stacks

Usage:
  ports <stack>                              show a stack (same as 'ports show <stack>')
  ports init [--recover]                     create the registry on a new server
  ports new <stack> [--fixed 80,443] [--block-size N] [--no-block] [--init-dir]
  ports list                                 list all stacks and reservations
  ports find <port> [--protocol tcp|udp]     find the stack reserving a port
  ports assign <stack> <name> <container-port> [--host-port N] [--protocol tcp|udp]
  ports reserve <stack> <port[-end][/proto],...> [--protocol] [--bind ADDR]
  ports add-block <stack> [--block-size N]   allocate another dynamic block
  ports release <stack> <assignment>         remove an assignment (keeps reservations)
  ports release <stack> --fixed PORT|--block remove a reservation (needs confirmation)
  ports check <stack>                        audit one stack against Compose and the host
  ports audit                                audit all stacks
  ports sync [--dry-run | --apply]           reconcile registry and .ports manifests
  ports delete <stack> --release-ports       remove a registration (never deletes data)
  ports version

Global options (accepted before or after the command):
  --config PATH       configuration file (default /etc/ports/config.json, env PORTS_CONFIG)
  --stack-root DIR    stack root (default /opt/stacks, env PORTS_STACK_ROOT)
  --registry PATH     registry file (default /var/lib/ports/registry.json, env PORTS_REGISTRY)
  --json              JSON output
  --yes               confirm destructive operations non-interactively

Exit codes:
  0 success   1 error   2 usage   3 conflict / audit violations
  4 audit incomplete (Compose invalid or Docker unavailable)   5 not found
  6 registry and manifest disagree (run 'ports sync')

Run 'ports help <command>' for details on a command.
`

func init() {
	register(&command{name: "init", usage: `Usage: ports init [--recover]

Create the global registry (and its directory) if it does not exist. Existing
registries are never overwritten. Run once on a new server, as a user that can
write the registry directory.

  --recover   if the registry is missing or corrupt, rebuild it from the .ports
              manifests under the stack root. A corrupt registry is preserved as
              registry.json.corrupt-<time>. Nothing is changed if the manifests
              conflict with each other.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		recover := fs.Bool("recover", false, "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 0, commands["init"].usage); err != nil {
				return 0, err
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			res, err := a.Init(*recover)
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(res)
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(e.stderr, "warning: %s\n", w)
			}
			switch {
			case res.Recovered:
				fmt.Fprintf(e.stdout, "Rebuilt registry %s from %d manifest(s): %s\n", res.RegistryPath, len(res.Imported), strings.Join(res.Imported, ", "))
				if res.Preserved != "" {
					fmt.Fprintf(e.stdout, "The previous (corrupt) registry was preserved as %s\n", res.Preserved)
				}
			case res.Created:
				fmt.Fprintf(e.stdout, "Created registry %s\n", res.RegistryPath)
			default:
				fmt.Fprintf(e.stdout, "Registry %s already exists and is valid; nothing to do\n", res.RegistryPath)
			}
			return ExitOK, nil
		}
	}})

	register(&command{name: "new", usage: `Usage: ports new <stack> [options]

Register a stack: reserve fixed ports (if any) and allocate a dynamic block,
then write <stack-root>/<stack>/.ports and the global registry.

  --fixed SPEC        fixed ports, e.g. 80,81,443 or 8000-8005 or 53/udp (repeatable)
  --protocol P        default protocol for --fixed entries: tcp (default), udp, both
  --bind ADDR         host bind address for --fixed entries (default 0.0.0.0 = all)
  --purpose TEXT      description stored with the fixed reservations
  --block-size N      size of the dynamic block (default from config, 5)
  --no-block          do not allocate a dynamic block (requires --fixed)
  --init-dir          create the stack directory if it does not exist
  --compose-file F    Compose file(s) for audits, relative to the stack dir (repeatable)
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		var fixed, files stringList
		fs.Var(&fixed, "fixed", "")
		fs.Var(&files, "compose-file", "")
		proto := fs.String("protocol", "tcp", "")
		bind := fs.String("bind", "", "")
		purpose := fs.String("purpose", "", "")
		size := fs.Int("block-size", 0, "")
		noBlock := fs.Bool("no-block", false, "")
		initDir := fs.Bool("init-dir", false, "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 1, commands["new"].usage); err != nil {
				return 0, err
			}
			if *size < 0 || (*size == 0 && flagSet(fs, "block-size")) {
				return 0, usagef("--block-size must be at least 1")
			}
			opt := app.NewOptions{BlockSize: *size, NoBlock: *noBlock, InitDir: *initDir, ComposeFiles: files}
			if len(fixed) > 0 {
				p, err := model.NormalizeProtocol(*proto, model.ProtoTCP, true)
				if err != nil {
					return 0, usagef("%v", err)
				}
				b, err := model.NormalizeBind(*bind)
				if err != nil {
					return 0, usagef("%v", err)
				}
				allocs, err := model.ParseFixedSpec(strings.Join(fixed, ","), p, b, *purpose)
				if err != nil {
					return 0, usagef("%v", err)
				}
				opt.Fixed = allocs
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			res, err := a.NewStack(pos[0], opt)
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(res)
			}
			e.warn(res.Warnings)
			fmt.Fprintf(e.stdout, "Registered stack %s (%s)\n", res.Stack.Name, res.Stack.Directory)
			for _, al := range res.Added {
				fmt.Fprintf(e.stdout, "  reserved %s\n", al)
			}
			return ExitOK, nil
		}
	}})

	register(&command{name: "show", usage: `Usage: ports <stack>  |  ports show <stack> [--no-compose] [--json]

Show a stack's reservations, intended assignments, effective Compose port
mappings (if Docker is available) and any registry/manifest discrepancies.

  --no-compose   do not run 'docker compose config'
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		noCompose := fs.Bool("no-compose", false, "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 1, commands["show"].usage); err != nil {
				return 0, err
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			info, err := a.Lookup(e.ctx, pos[0], !*noCompose)
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(info)
			}
			printStackInfo(e.stdout, info)
			return ExitOK, nil
		}
	}})

	register(&command{name: "list", usage: `Usage: ports list [--json]

List all registered stacks with their fixed reservations and dynamic blocks.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 0, commands["list"].usage); err != nil {
				return 0, err
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			reg, err := a.List()
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				stacks := reg.SortedStacks()
				return e.emitJSON(map[string]any{"version": reg.Version, "stacks": stacks})
			}
			printList(e.stdout, reg)
			return ExitOK, nil
		}
	}})
	commands["ls"] = commands["list"]

	register(&command{name: "find", usage: `Usage: ports find <port> [--protocol tcp|udp|both] [--bind ADDR] [--json]

Show which stack(s) reserve a host port. Exit code 5 if none does. Absence from
the registry does not mean the port is free on the host.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		proto := fs.String("protocol", "", "")
		bind := fs.String("bind", "", "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 1, commands["find"].usage); err != nil {
				return 0, err
			}
			port, err := parsePortArg("port", pos[0])
			if err != nil {
				return 0, err
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			matches, err := a.Find(port, *proto, *bind)
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(map[string]any{"port": port, "matches": matches})
			}
			printFind(e.stdout, port, matches)
			return ExitOK, nil
		}
	}})

	register(&command{name: "assign", usage: `Usage: ports assign <stack> <assignment-name> <container-port> [options]

Record that a reserved host port is intended for a container port. Without
--host-port the lowest unassigned reserved port is chosen. Compose files are
not modified; add the mapping to compose.yml yourself.

  --host-port N    use this reserved host port
  --protocol P     tcp (default) or udp
  --bind ADDR      host bind address (default 0.0.0.0 = all addresses)
  --purpose TEXT   free-form description
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		hostPort := fs.Int("host-port", 0, "")
		proto := fs.String("protocol", "tcp", "")
		bind := fs.String("bind", "", "")
		purpose := fs.String("purpose", "", "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 3, commands["assign"].usage); err != nil {
				return 0, err
			}
			cport, err := parsePortArg("container port", pos[2])
			if err != nil {
				return 0, err
			}
			if flagSet(fs, "host-port") {
				if err := model.ValidatePort(*hostPort); err != nil {
					return 0, usagef("--host-port: %v", err)
				}
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			res, err := a.Assign(pos[0], pos[1], cport, app.AssignOptions{HostPort: *hostPort, Protocol: *proto, Bind: *bind, Purpose: *purpose})
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(res)
			}
			e.warn(res.Warnings)
			as := res.Assigned
			fmt.Fprintf(e.stdout, "Assigned %s in stack %s\n", as, res.Stack.Name)
			fmt.Fprintf(e.stdout, "Compose port entry: %s\n", composeEntry(*as))
			return ExitOK, nil
		}
	}})

	register(&command{name: "reserve", usage: `Usage: ports reserve <stack> <port-spec> [options]

Add fixed reservations to an existing stack. port-spec is a comma-separated
list such as 8096, 80,443, 8000-8005 or 53/udp. Conflicts are checked first.

  --protocol P     default protocol: tcp (default), udp or both
  --bind ADDR      host bind address (default 0.0.0.0 = all addresses)
  --purpose TEXT   free-form description
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		proto := fs.String("protocol", "tcp", "")
		bind := fs.String("bind", "", "")
		purpose := fs.String("purpose", "", "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 2, commands["reserve"].usage); err != nil {
				return 0, err
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			res, err := a.Reserve(pos[0], pos[1], app.ReserveOptions{Protocol: *proto, Bind: *bind, Purpose: *purpose})
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(res)
			}
			e.warn(res.Warnings)
			for _, al := range res.Added {
				fmt.Fprintf(e.stdout, "Reserved %s for stack %s\n", al, res.Stack.Name)
			}
			return ExitOK, nil
		}
	}})

	register(&command{name: "add-block", usage: `Usage: ports add-block <stack> [--block-size N]

Allocate an additional dynamic block for an existing stack.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		size := fs.Int("block-size", 0, "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 1, commands["add-block"].usage); err != nil {
				return 0, err
			}
			if *size < 0 || (*size == 0 && flagSet(fs, "block-size")) {
				return 0, usagef("--block-size must be at least 1")
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			res, err := a.AddBlock(pos[0], *size)
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(res)
			}
			for _, al := range res.Added {
				fmt.Fprintf(e.stdout, "Reserved %s for stack %s\n", al, res.Stack.Name)
			}
			return ExitOK, nil
		}
	}})

	register(&command{name: "release", usage: `Usage:
  ports release <stack> <assignment-name>
  ports release <stack> --fixed PORT [--protocol P] [--bind ADDR] [--yes]
  ports release <stack> --block [--yes]

The first form removes an assignment; the reserved port stays with the stack.
The other forms free reservations so other stacks may receive them; they require
confirmation and refuse while an assignment still uses the reservation.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		fixed := fs.Int("fixed", 0, "")
		block := fs.Bool("block", false, "")
		proto := fs.String("protocol", "", "")
		bind := fs.String("bind", "", "")
		return func(pos []string) (int, error) {
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			var res *app.Result
			reservation := flagSet(fs, "fixed") || *block
			switch {
			case flagSet(fs, "fixed") && *block:
				return 0, usagef("--fixed and --block cannot be combined")
			case reservation:
				if err := needArgs(pos, 1, commands["release"].usage); err != nil {
					return 0, err
				}
				res, err = a.ReleaseReservation(pos[0], *fixed, *block, *proto, *bind)
			default:
				if err := needArgs(pos, 2, commands["release"].usage); err != nil {
					return 0, err
				}
				if flagSet(fs, "protocol") || flagSet(fs, "bind") {
					return 0, usagef("--protocol and --bind only apply with --fixed")
				}
				res, err = a.Release(pos[0], pos[1])
			}
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(res)
			}
			if reservation {
				for _, al := range res.Removed {
					fmt.Fprintf(e.stdout, "Released %s from stack %s\n", al, res.Stack.Name)
				}
			} else {
				fmt.Fprintf(e.stdout, "Released assignment %s from stack %s; host port %d remains reserved for the stack\n", res.Assigned.Name, res.Stack.Name, res.Assigned.HostPort)
			}
			return ExitOK, nil
		}
	}})

	auditFlags := func(fs *flag.FlagSet) (noHost, record *bool) {
		return fs.Bool("no-host", false, ""), fs.Bool("record", false, "")
	}
	register(&command{name: "check", usage: `Usage: ports check <stack> [--compose-file F]... [--no-host] [--record] [--json]

Compare a stack's reservations and assignments with its effective Compose
configuration ('docker compose config') and with listening host sockets.

  --compose-file F  use these Compose files instead of the registered ones (repeatable)
  --no-host         skip host socket inspection
  --record          store the observed mappings in the stack's .ports manifest

Exit codes: 0 clean (warnings allowed), 3 violations, 4 Compose invalid or
Docker unavailable.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		var files stringList
		fs.Var(&files, "compose-file", "")
		noHost, record := auditFlags(fs)
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 1, commands["check"].usage); err != nil {
				return 0, err
			}
			return e.runAudit(app.AuditOptions{Stacks: pos, ComposeFiles: files, Host: !*noHost, Record: *record})
		}
	}})
	register(&command{name: "audit", usage: `Usage: ports audit [--no-host] [--record] [--json]

Audit every registered stack: registry consistency, manifests, Compose
mappings and host listeners. Exit codes as for 'ports check'.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		noHost, record := auditFlags(fs)
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 0, commands["audit"].usage); err != nil {
				return 0, err
			}
			return e.runAudit(app.AuditOptions{Host: !*noHost, Record: *record})
		}
	}})

	register(&command{name: "sync", usage: `Usage: ports sync [--dry-run | --apply] [--json]

Compare the registry with every .ports manifest and report discrepancies with
proposed repairs. The default is a dry run. With --apply only safe,
unambiguous repairs are made:
  - missing, corrupt or differing manifests are rewritten from the
    authoritative registry (existing files are preserved as .ports.bak-<time>);
  - valid unregistered manifests that do not conflict are imported.
Exit code 6 if discrepancies remain.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		dry := fs.Bool("dry-run", false, "")
		apply := fs.Bool("apply", false, "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 0, commands["sync"].usage); err != nil {
				return 0, err
			}
			if *dry && *apply {
				return 0, usagef("--dry-run and --apply cannot be combined")
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			rep, err := a.Sync(*apply)
			if err != nil {
				return 0, err
			}
			code := ExitOK
			if rep.Pending() {
				code = ExitInconsistent
			}
			if e.jsonOut {
				if _, err := e.emitJSON(rep); err != nil {
					return ExitError, err
				}
				return code, nil
			}
			printSync(e.stdout, rep)
			return code, nil
		}
	}})

	register(&command{name: "delete", usage: `Usage: ports delete <stack> --release-ports [--yes]

Remove a stack's registration and free all of its reservations. The stack
directory, compose files and application data are never deleted; the manifest
is renamed to .ports.deleted-<time>. Without --release-ports nothing is changed
and the reservations that would be freed are shown.
`, setup: func(fs *flag.FlagSet, e *env) func([]string) (int, error) {
		release := fs.Bool("release-ports", false, "")
		return func(pos []string) (int, error) {
			if err := needArgs(pos, 1, commands["delete"].usage); err != nil {
				return 0, err
			}
			a, err := e.app()
			if err != nil {
				return 0, err
			}
			res, err := a.Delete(pos[0], *release)
			if err != nil {
				return 0, err
			}
			if e.jsonOut {
				return e.emitJSON(res)
			}
			fmt.Fprintf(e.stdout, "Deleted registration of stack %s; its reservations are now free. Data in %s was not touched.\n", res.Stack.Name, res.Stack.Directory)
			if res.ManifestArchived != "" {
				fmt.Fprintf(e.stdout, "Manifest archived as %s\n", res.ManifestArchived)
			}
			return ExitOK, nil
		}
	}})
}

func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func (e *env) warn(ws []string) {
	for _, w := range ws {
		fmt.Fprintf(e.stderr, "warning: %s\n", w)
	}
}

func (e *env) runAudit(opt app.AuditOptions) (int, error) {
	a, err := e.app()
	if err != nil {
		return 0, err
	}
	rep, err := a.Audit(e.ctx, opt)
	if err != nil && rep == nil {
		return 0, err
	}
	if e.jsonOut {
		if _, jerr := e.emitJSON(rep); jerr != nil {
			return ExitError, jerr
		}
	} else {
		printAudit(e.stdout, rep)
	}
	if err != nil {
		return 0, err
	}
	return auditExit(rep), nil
}

func composeEntry(as model.Assignment) string {
	host := fmt.Sprintf("%d", as.HostPort)
	if as.BindAddress != model.WildcardIPv4 {
		b := as.BindAddress
		if strings.Contains(b, ":") {
			b = "[" + b + "]"
		}
		host = b + ":" + host
	}
	entry := fmt.Sprintf("%s:%d", host, as.ContainerPort)
	if as.Protocol == model.ProtoUDP {
		entry += "/udp"
	}
	return fmt.Sprintf("\"%s\"", entry)
}
