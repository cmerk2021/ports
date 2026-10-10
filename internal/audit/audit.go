// Package audit compares registered reservations and assignments with the
// effective Docker Compose configuration and with host listeners.
package audit

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/cmerk2021/ports/internal/compose"
	"github.com/cmerk2021/ports/internal/hostports"
	"github.com/cmerk2021/ports/internal/model"
)

// Severity classifies findings.
type Severity string

// Severities, from most to least serious. Violations are allocation problems
// that need action; errors are configuration/inspection problems that
// prevented a complete audit.
const (
	SevViolation Severity = "violation"
	SevError     Severity = "error"
	SevWarning   Severity = "warning"
	SevInfo      Severity = "info"
)

// Finding codes.
const (
	CodeOutsideAllocation  = "outside-allocation"
	CodeForeignReservation = "reserved-by-other-stack"
	CodeUnassignedMapping  = "unassigned-mapping"
	CodeAssignmentMismatch = "assignment-mismatch"
	CodeDuplicatePublish   = "duplicate-publish"
	CodeAssignmentUnused   = "assignment-not-published"
	CodeComposeInvalid     = "compose-invalid"
	CodeDockerUnavailable  = "docker-unavailable"
	CodeUnresolvedVariable = "unresolved-variable"
	CodeEphemeralPort      = "ephemeral-port"
	CodeHostNetwork        = "host-network"
	CodeHostListener       = "host-listener"
	CodeHostListenerOwn    = "host-listener-published"
	CodeUnreservedListener = "unreserved-listener"
	CodeHostUnavailable    = "host-inspection-unavailable"
	CodeManifestMismatch   = "manifest-mismatch"
	CodeManifestProblem    = "manifest-problem"
	CodeRegistryConflict   = "registry-conflict"
)

// Finding is one audit result.
type Finding struct {
	Stack    string   `json:"stack,omitempty"`
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Service  string   `json:"service,omitempty"`
	Port     int      `json:"port,omitempty"`
	Protocol string   `json:"protocol,omitempty"`
}

func (f Finding) String() string {
	return fmt.Sprintf("[%s] %s: %s", f.Severity, f.Code, f.Message)
}

// StackReport is the audit result for one stack.
type StackReport struct {
	Stack         string            `json:"stack"`
	Directory     string            `json:"directory"`
	ComposeStatus string            `json:"compose_status"`
	Mappings      []compose.Mapping `json:"mappings"`
	Findings      []Finding         `json:"findings"`
}

// Compose status values.
const (
	ComposeOK          = "ok"
	ComposeInvalid     = "invalid"
	ComposeUnavailable = "unavailable"
)

// Report is a complete audit.
type Report struct {
	Stacks   []StackReport `json:"stacks"`
	Global   []Finding     `json:"global_findings"`
	Summary  Summary       `json:"summary"`
	HostScan string        `json:"host_scan"`
}

// Summary counts findings by severity.
type Summary struct {
	Violations int `json:"violations"`
	Errors     int `json:"errors"`
	Warnings   int `json:"warnings"`
	Info       int `json:"info"`
}

// Options configures an audit.
type Options struct {
	Runner       compose.Runner
	ComposeFiles []string            // default files when a stack has none
	Host         *hostports.Snapshot // nil disables host checks
}

// CheckStack audits one stack against Compose and host listeners.
func CheckStack(ctx context.Context, reg *model.Registry, s *model.Stack, opt Options) StackReport {
	rep := StackReport{Stack: s.Name, Directory: s.Directory, Mappings: []compose.Mapping{}, Findings: []Finding{}}
	add := func(sev Severity, code, msg string) *Finding {
		rep.Findings = append(rep.Findings, Finding{Stack: s.Name, Severity: sev, Code: code, Message: msg})
		return &rep.Findings[len(rep.Findings)-1]
	}

	files := s.ComposeFiles
	if len(files) == 0 {
		files = opt.ComposeFiles
	}
	res, err := compose.Inspect(ctx, opt.Runner, s.Directory, files)
	var cfgErr *compose.ConfigError
	switch {
	case err == nil:
		rep.ComposeStatus = ComposeOK
	case errors.Is(err, compose.ErrUnavailable):
		rep.ComposeStatus = ComposeUnavailable
		add(SevError, CodeDockerUnavailable, err.Error()+"; Compose mappings were not checked")
	case errors.As(err, &cfgErr):
		rep.ComposeStatus = ComposeInvalid
		add(SevError, CodeComposeInvalid, cfgErr.Message)
	default:
		rep.ComposeStatus = ComposeInvalid
		add(SevError, CodeComposeInvalid, err.Error())
	}

	if res != nil {
		rep.Mappings = res.Mappings
		for _, n := range res.Notes {
			switch n.Kind {
			case compose.NoteUnresolvedVariable:
				add(SevError, CodeUnresolvedVariable, n.Message)
			case compose.NoteEphemeralPort:
				add(SevWarning, CodeEphemeralPort, n.Message).Service = n.Service
			case compose.NoteHostNetwork:
				add(SevWarning, CodeHostNetwork, fmt.Sprintf("service %q %s", n.Service, n.Message)).Service = n.Service
			}
		}
		checkMappings(reg, s, res.Mappings, add)
	}

	if opt.Host != nil {
		checkHost(s, rep.Mappings, rep.ComposeStatus == ComposeOK, opt.Host, add)
	}
	return rep
}

type addFunc func(sev Severity, code, msg string) *Finding

func checkMappings(reg *model.Registry, s *model.Stack, mappings []compose.Mapping, add addFunc) {
	for i, m := range mappings {
		mark := func(f *Finding) { f.Service, f.Port, f.Protocol = m.Service, m.HostPort, m.Protocol }
		for _, o := range mappings[:i] {
			if o.HostPort == m.HostPort && o.Protocol == m.Protocol && model.BindsOverlap(o.BindAddress, m.BindAddress) {
				mark(add(SevViolation, CodeDuplicatePublish, fmt.Sprintf("host port %d/%s is published twice (services %q and %q)", m.HostPort, m.Protocol, o.Service, m.Service)))
			}
		}
		if _, ok := s.Covering(m.HostPort, m.Protocol, m.BindAddress); !ok {
			probe := model.Allocation{Type: model.TypeFixed, Start: m.HostPort, End: m.HostPort, Protocol: m.Protocol, BindAddress: m.BindAddress}
			if cs := reg.ConflictsWith(probe, s.Name); len(cs) > 0 {
				mark(add(SevViolation, CodeForeignReservation, fmt.Sprintf("service %q publishes %s which is reserved by stack %q (%s)", m.Service, describe(m), cs[0].StackB, cs[0].B)))
			} else {
				mark(add(SevViolation, CodeOutsideAllocation, fmt.Sprintf("service %q publishes %s which is outside the stack's reservations (reserve it with `ports reserve %s %d`)", m.Service, describe(m), s.Name, m.HostPort)))
			}
			continue
		}
		idx := -1
		for j, as := range s.Assignments {
			if as.HostPort == m.HostPort && as.Protocol == m.Protocol && model.BindsOverlap(as.BindAddress, m.BindAddress) {
				idx = j
				break
			}
		}
		if idx < 0 {
			mark(add(SevViolation, CodeUnassignedMapping, fmt.Sprintf("service %q publishes %s which is reserved but not registered as an assignment (run `ports assign %s <name> %d --host-port %d --protocol %s`)", m.Service, describe(m), s.Name, m.ContainerPort, m.HostPort, m.Protocol)))
			continue
		}
		as := s.Assignments[idx]
		if as.ContainerPort != m.ContainerPort || !sameBind(as.BindAddress, m.BindAddress) {
			mark(add(SevViolation, CodeAssignmentMismatch, fmt.Sprintf("assignment %q expects %s:%d -> %d/%s but service %q publishes %s", as.Name, as.BindAddress, as.HostPort, as.ContainerPort, as.Protocol, m.Service, describe(m))))
		}
	}
	for _, as := range s.Assignments {
		found := false
		for _, m := range mappings {
			if as.HostPort == m.HostPort && as.Protocol == m.Protocol && model.BindsOverlap(as.BindAddress, m.BindAddress) {
				found = true
				break
			}
		}
		if !found {
			f := add(SevWarning, CodeAssignmentUnused, fmt.Sprintf("assignment %q (%d/%s) is not published in the Compose configuration (release it with `ports release %s %s` if it is no longer needed)", as.Name, as.HostPort, as.Protocol, s.Name, as.Name))
			f.Port, f.Protocol = as.HostPort, as.Protocol
		}
	}
}

func sameBind(a, b string) bool {
	if model.IsWildcard(a) && model.IsWildcard(b) {
		return true
	}
	na, _ := model.NormalizeBind(a)
	nb, _ := model.NormalizeBind(b)
	return na == nb
}

func describe(m compose.Mapping) string {
	return fmt.Sprintf("%s:%d -> %d/%s", m.BindAddress, m.HostPort, m.ContainerPort, m.Protocol)
}

// checkHost reports host listeners on ports reserved by the stack. Listener
// ownership cannot be determined, so a listener on a port the stack itself
// publishes is reported as informational (it is probably the running stack)
// and any other listener as a warning.
func checkHost(s *model.Stack, mappings []compose.Mapping, composeOK bool, host *hostports.Snapshot, add addFunc) {
	for _, l := range host.Listeners {
		probe := model.Allocation{Type: model.TypeFixed, Start: l.Port, End: l.Port, Protocol: l.Protocol, BindAddress: l.BindAddress}
		var owner *model.Allocation
		for i := range s.Allocations {
			if s.Allocations[i].Conflicts(probe) {
				owner = &s.Allocations[i]
				break
			}
		}
		if owner == nil {
			continue
		}
		published := false
		for _, m := range mappings {
			if m.HostPort == l.Port && m.Protocol == l.Protocol && model.BindsOverlap(m.BindAddress, l.BindAddress) {
				published = true
				break
			}
		}
		var f *Finding
		switch {
		case published:
			f = add(SevInfo, CodeHostListenerOwn, fmt.Sprintf("%s:%d/%s is in use on the host; the stack publishes this port, so it is probably the running stack (owner not verified)", l.BindAddress, l.Port, l.Protocol))
		case composeOK:
			f = add(SevWarning, CodeHostListener, fmt.Sprintf("%s:%d/%s is reserved for this stack (%s) but is in use on the host by an unknown process; the stack does not publish it", l.BindAddress, l.Port, l.Protocol, owner.Type))
		default:
			f = add(SevWarning, CodeHostListener, fmt.Sprintf("%s:%d/%s is reserved for this stack (%s) and is in use on the host by an unknown process (Compose could not be inspected to tell whether it is this stack)", l.BindAddress, l.Port, l.Protocol, owner.Type))
		}
		f.Port, f.Protocol = l.Port, l.Protocol
	}
}

// UnreservedListeners reports host listeners inside the dynamic range that no
// stack reserves; they would make future allocations collide at runtime.
func UnreservedListeners(reg *model.Registry, rng model.Range, host *hostports.Snapshot) []Finding {
	var out []Finding
	for _, l := range host.Listeners {
		if l.Port < rng.Start || l.Port > rng.End {
			continue
		}
		probe := model.Allocation{Type: model.TypeFixed, Start: l.Port, End: l.Port, Protocol: l.Protocol, BindAddress: l.BindAddress}
		if len(reg.ConflictsWith(probe, "")) > 0 {
			continue
		}
		out = append(out, Finding{Severity: SevWarning, Code: CodeUnreservedListener, Port: l.Port, Protocol: l.Protocol,
			Message: fmt.Sprintf("%s:%d/%s is in use on the host inside the dynamic range but is not reserved by any stack (new blocks will skip it)", l.BindAddress, l.Port, l.Protocol)})
	}
	return out
}

// Finalize sorts findings and computes the summary.
func (r *Report) Finalize() {
	sev := map[Severity]int{SevViolation: 0, SevError: 1, SevWarning: 2, SevInfo: 3}
	sortF := func(fs []Finding) {
		sort.SliceStable(fs, func(i, j int) bool { return sev[fs[i].Severity] < sev[fs[j].Severity] })
	}
	r.Summary = Summary{}
	count := func(fs []Finding) {
		for _, f := range fs {
			switch f.Severity {
			case SevViolation:
				r.Summary.Violations++
			case SevError:
				r.Summary.Errors++
			case SevWarning:
				r.Summary.Warnings++
			default:
				r.Summary.Info++
			}
		}
	}
	if r.Global == nil {
		r.Global = []Finding{}
	}
	sortF(r.Global)
	count(r.Global)
	for i := range r.Stacks {
		sortF(r.Stacks[i].Findings)
		count(r.Stacks[i].Findings)
	}
}
