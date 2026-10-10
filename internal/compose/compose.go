// Package compose inspects the effective Docker Compose configuration of a
// stack by running `docker compose config --format json` and extracting the
// published port mappings from the normalized output.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cmerk2021/ports/internal/model"
)

// ErrUnavailable means Docker / Docker Compose could not be executed.
var ErrUnavailable = errors.New("docker compose is unavailable")

// ConfigError means Compose rejected the stack configuration.
type ConfigError struct {
	Message string
}

func (e *ConfigError) Error() string { return "invalid compose configuration: " + e.Message }

// Mapping is one published port in the effective configuration.
type Mapping struct {
	Service       string `json:"service"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
	BindAddress   string `json:"bind_address"`
}

func (m Mapping) String() string {
	return fmt.Sprintf("%s: %s:%d -> %d/%s", m.Service, m.BindAddress, m.HostPort, m.ContainerPort, m.Protocol)
}

// Note is a non-fatal observation about the configuration.
type Note struct {
	Kind    string `json:"kind"`
	Service string `json:"service,omitempty"`
	Message string `json:"message"`
}

// Note kinds.
const (
	NoteUnresolvedVariable = "unresolved-variable"
	NoteEphemeralPort      = "ephemeral-port"
	NoteHostNetwork        = "host-network"
)

// Result is the parsed effective configuration.
type Result struct {
	Mappings []Mapping `json:"mappings"`
	Notes    []Note    `json:"notes,omitempty"`
}

// Runner executes `docker compose config`.
type Runner interface {
	Config(ctx context.Context, dir string, files []string) (stdout, stderr []byte, err error)
}

// ExecRunner runs the real docker CLI.
type ExecRunner struct {
	Command string        // docker executable (default "docker")
	Timeout time.Duration // per invocation (default 60s)
}

// Config implements Runner.
func (r ExecRunner) Config(ctx context.Context, dir string, files []string) ([]byte, []byte, error) {
	command := r.Command
	if command == "" {
		command = "docker"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s not found in PATH", ErrUnavailable, command)
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"compose", "--project-directory", dir}
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(dir, f)
		}
		args = append(args, "-f", f)
	}
	args = append(args, "config", "--format", "json")
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, stderr.Bytes(), fmt.Errorf("%w: `docker compose config` timed out after %s", ErrUnavailable, timeout)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// Inspect runs the runner and parses its output.
func Inspect(ctx context.Context, r Runner, dir string, files []string) (*Result, error) {
	stdout, stderr, err := r.Config(ctx, dir, files)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		msg := cleanStderr(stderr)
		if strings.Contains(msg, "is not a docker command") || strings.Contains(msg, "unknown command") {
			return nil, fmt.Errorf("%w: the docker compose plugin is not installed", ErrUnavailable)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, &ConfigError{Message: msg}
	}
	res, err := Parse(stdout)
	if err != nil {
		return nil, err
	}
	for _, v := range UnresolvedVariables(stderr) {
		res.Notes = append(res.Notes, Note{Kind: NoteUnresolvedVariable, Message: fmt.Sprintf("variable %q is not set (Compose substituted a blank string)", v)})
	}
	return res, nil
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func cleanStderr(b []byte) string {
	s := ansiRe.ReplaceAllString(string(b), "")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.Contains(l, "level=warning") {
			continue
		}
		lines = append(lines, l)
		if len(lines) == 5 {
			break
		}
	}
	return strings.Join(lines, "; ")
}

var unsetVarRe = regexp.MustCompile(`The \\?"([A-Za-z_][A-Za-z0-9_]*)\\?" variable is not set`)

// UnresolvedVariables extracts unset-variable warnings from Compose stderr.
func UnresolvedVariables(stderr []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range unsetVarRe.FindAllSubmatch(stderr, -1) {
		v := string(m[1])
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

type rawPort struct {
	Target    json.RawMessage `json:"target"`
	Published json.RawMessage `json:"published"`
	Protocol  string          `json:"protocol"`
	HostIP    string          `json:"host_ip"`
	Mode      string          `json:"mode"`
}

type rawService struct {
	NetworkMode string    `json:"network_mode"`
	Ports       []rawPort `json:"ports"`
}

type rawProject struct {
	Services map[string]rawService `json:"services"`
}

// Parse extracts published port mappings from normalized
// `docker compose config --format json` output.
func Parse(data []byte) (*Result, error) {
	var p rawProject
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("cannot parse `docker compose config` JSON output: %v", err)}
	}
	res := &Result{Mappings: []Mapping{}}
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := p.Services[name]
		if svc.NetworkMode == "host" {
			res.Notes = append(res.Notes, Note{Kind: NoteHostNetwork, Service: name,
				Message: "uses network_mode: host; its listening ports cannot be determined from Compose"})
		}
		for _, rp := range svc.Ports {
			target, err := scalarString(rp.Target)
			if err != nil {
				return nil, &ConfigError{Message: fmt.Sprintf("service %q: invalid target port: %v", name, err)}
			}
			cStart, cEnd, err := model.ParsePortRange(target)
			if err != nil {
				return nil, &ConfigError{Message: fmt.Sprintf("service %q: invalid target port %q: %v", name, target, err)}
			}
			published, err := scalarString(rp.Published)
			if err != nil {
				return nil, &ConfigError{Message: fmt.Sprintf("service %q: invalid published port: %v", name, err)}
			}
			proto := strings.ToLower(rp.Protocol)
			if proto == "" {
				proto = model.ProtoTCP
			}
			if proto != model.ProtoTCP && proto != model.ProtoUDP {
				return nil, &ConfigError{Message: fmt.Sprintf("service %q: unsupported protocol %q", name, rp.Protocol)}
			}
			bind, err := model.NormalizeBind(rp.HostIP)
			if err != nil {
				return nil, &ConfigError{Message: fmt.Sprintf("service %q: %v", name, err)}
			}
			if published == "" {
				res.Notes = append(res.Notes, Note{Kind: NoteEphemeralPort, Service: name,
					Message: fmt.Sprintf("container port %s/%s is published on a random ephemeral host port", target, proto)})
				continue
			}
			hStart, hEnd, err := model.ParsePortRange(published)
			if err != nil {
				return nil, &ConfigError{Message: fmt.Sprintf("service %q: invalid published port %q: %v", name, published, err)}
			}
			for h := hStart; h <= hEnd; h++ {
				c := cStart
				if cEnd > cStart {
					c = cStart + (h - hStart)
					if c > cEnd {
						c = cEnd
					}
				}
				res.Mappings = append(res.Mappings, Mapping{Service: name, HostPort: h, ContainerPort: c, Protocol: proto, BindAddress: bind})
			}
		}
	}
	return res, nil
}

func scalarString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s), nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if _, err := strconv.Atoi(n.String()); err != nil {
			return "", fmt.Errorf("non-integer port %s", n)
		}
		return n.String(), nil
	}
	return "", fmt.Errorf("unexpected value %s", string(raw))
}
