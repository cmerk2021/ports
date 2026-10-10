package audit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cmerk2021/ports/internal/compose"
	"github.com/cmerk2021/ports/internal/hostports"
	"github.com/cmerk2021/ports/internal/model"
)

type fakeRunner struct {
	stdout string
	err    error
}

func (f fakeRunner) Config(context.Context, string, []string) ([]byte, []byte, error) {
	return []byte(f.stdout), nil, f.err
}

func registry() *model.Registry {
	r := model.NewRegistry(time.Unix(0, 0))
	r.Stacks["jellyfin"] = &model.Stack{Name: "jellyfin", Directory: "/s/jellyfin",
		Allocations: []model.Allocation{
			{Type: "fixed", Start: 8096, End: 8096, Protocol: "tcp", BindAddress: "0.0.0.0"},
			{Type: "block", Start: 12000, End: 12004, Protocol: "both", BindAddress: "0.0.0.0"},
		},
		Assignments: []model.Assignment{
			{Name: "web", HostPort: 8096, ContainerPort: 8096, Protocol: "tcp", BindAddress: "0.0.0.0", Status: "assigned"},
			{Name: "dlna", HostPort: 12000, ContainerPort: 1900, Protocol: "udp", BindAddress: "0.0.0.0", Status: "assigned"},
		}}
	r.Stacks["npm"] = &model.Stack{Name: "npm", Directory: "/s/npm",
		Allocations: []model.Allocation{{Type: "fixed", Start: 80, End: 80, Protocol: "tcp", BindAddress: "0.0.0.0"}}}
	return r
}

func ports(entries string) string {
	return `{"services":{"jf":{"ports":[` + entries + `]}}}`
}

func codes(rep StackReport) map[string]int {
	m := map[string]int{}
	for _, f := range rep.Findings {
		m[f.Code]++
	}
	return m
}

func TestCheckStackClean(t *testing.T) {
	r := registry()
	out := ports(`{"target":8096,"published":"8096","protocol":"tcp"},{"target":1900,"published":"12000","protocol":"udp"}`)
	rep := CheckStack(context.Background(), r, r.Stacks["jellyfin"], Options{Runner: fakeRunner{stdout: out}})
	if len(rep.Findings) != 0 || rep.ComposeStatus != ComposeOK || len(rep.Mappings) != 2 {
		t.Fatalf("expected clean report, got %+v", rep)
	}
}

func TestCheckStackViolations(t *testing.T) {
	r := registry()
	out := ports(`{"target":8000,"published":"8096","protocol":"tcp"},` + // container port mismatch
		`{"target":1,"published":"12001","protocol":"tcp"},` + // unassigned but reserved
		`{"target":2,"published":"7000"},` + // outside allocations
		`{"target":3,"published":"80"},` + // reserved by npm
		`{"target":4,"published":"12001","protocol":"tcp","host_ip":"127.0.0.1"}`) // duplicate publish
	rep := CheckStack(context.Background(), r, r.Stacks["jellyfin"], Options{Runner: fakeRunner{stdout: out}})
	got := codes(rep)
	for _, c := range []string{CodeAssignmentMismatch, CodeOutsideAllocation, CodeForeignReservation, CodeDuplicatePublish, CodeAssignmentUnused} {
		if got[c] == 0 {
			t.Errorf("missing %s in %v", c, got)
		}
	}
	if got[CodeUnassignedMapping] != 2 {
		t.Errorf("unassigned-mapping = %d, want 2 (%v)", got[CodeUnassignedMapping], rep.Findings)
	}
	full := &Report{Stacks: []StackReport{rep}}
	full.Finalize()
	if full.Summary.Violations == 0 || full.Stacks[0].Findings[0].Severity != SevViolation {
		t.Fatalf("summary %+v", full.Summary)
	}
}

func TestCheckStackSpecificBindReservation(t *testing.T) {
	r := registry()
	r.Stacks["lan"] = &model.Stack{Name: "lan", Directory: "/s/lan",
		Allocations: []model.Allocation{{Type: "fixed", Start: 9000, End: 9000, Protocol: "tcp", BindAddress: "192.168.1.10"}}}
	run := func(entry string) map[string]int {
		return codes(CheckStack(context.Background(), r, r.Stacks["lan"], Options{Runner: fakeRunner{stdout: ports(entry)}}))
	}
	if c := run(`{"target":1,"published":"9000","host_ip":"0.0.0.0"}`); c[CodeOutsideAllocation] != 1 {
		t.Fatalf("wildcard publish of specific reservation must be outside: %v", c)
	}
	if c := run(`{"target":1,"published":"9000","host_ip":"192.168.1.10"}`); c[CodeUnassignedMapping] != 1 || c[CodeOutsideAllocation] != 0 {
		t.Fatalf("matching specific publish: %v", c)
	}
}

func TestCheckStackConfigErrors(t *testing.T) {
	r := registry()
	s := r.Stacks["jellyfin"]
	rep := CheckStack(context.Background(), r, s, Options{Runner: fakeRunner{err: &compose.ConfigError{Message: "bad yaml"}}})
	if rep.ComposeStatus != ComposeInvalid || codes(rep)[CodeComposeInvalid] != 1 {
		t.Fatalf("%+v", rep)
	}
	rep = CheckStack(context.Background(), r, s, Options{Runner: fakeRunner{err: errors.Join(compose.ErrUnavailable, errors.New("no docker"))}})
	if rep.ComposeStatus != ComposeUnavailable || codes(rep)[CodeDockerUnavailable] != 1 {
		t.Fatalf("%+v", rep)
	}
	full := &Report{Stacks: []StackReport{rep}}
	full.Finalize()
	if full.Summary.Errors != 1 || full.Summary.Violations != 0 {
		t.Fatalf("config errors must be distinguished from violations: %+v", full.Summary)
	}
}

func TestCheckStackHostListeners(t *testing.T) {
	r := registry()
	host := &hostports.Snapshot{Listeners: []hostports.Listener{
		{Protocol: "tcp", BindAddress: "::", Port: 8096},         // published by the stack
		{Protocol: "tcp", BindAddress: "127.0.0.1", Port: 12003}, // reserved, not published
		{Protocol: "tcp", BindAddress: "0.0.0.0", Port: 13000},   // unrelated
	}}
	out := ports(`{"target":8096,"published":"8096"},{"target":1900,"published":"12000","protocol":"udp"}`)
	rep := CheckStack(context.Background(), r, r.Stacks["jellyfin"], Options{Runner: fakeRunner{stdout: out}, Host: host})
	c := codes(rep)
	if c[CodeHostListenerOwn] != 1 || c[CodeHostListener] != 1 {
		t.Fatalf("got %v", rep.Findings)
	}
	un := UnreservedListeners(r, model.Range{Start: 12000, End: 29999}, host)
	if len(un) != 1 || un[0].Port != 13000 {
		t.Fatalf("unreserved = %v", un)
	}
}
