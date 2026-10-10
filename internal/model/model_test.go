package model

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fixed(start, end int, proto, bind string) Allocation {
	return Allocation{Type: TypeFixed, Start: start, End: end, Protocol: proto, BindAddress: bind}
}

func block(start, end int) Allocation {
	return Allocation{Type: TypeBlock, Start: start, End: end, Protocol: ProtoBoth, BindAddress: WildcardIPv4}
}

func TestConflicts(t *testing.T) {
	cases := []struct {
		name string
		a, b Allocation
		want bool
	}{
		{"same tcp port", fixed(80, 80, "tcp", "0.0.0.0"), fixed(80, 80, "tcp", "0.0.0.0"), true},
		{"tcp vs udp same port", fixed(53, 53, "tcp", "0.0.0.0"), fixed(53, 53, "udp", "0.0.0.0"), false},
		{"both vs udp", fixed(53, 53, "both", "0.0.0.0"), fixed(53, 53, "udp", "0.0.0.0"), true},
		{"adjacent", fixed(80, 80, "tcp", "0.0.0.0"), fixed(81, 81, "tcp", "0.0.0.0"), false},
		{"range overlap", fixed(8000, 8005, "tcp", "0.0.0.0"), fixed(8005, 8010, "tcp", "0.0.0.0"), true},
		{"wildcard vs specific", fixed(80, 80, "tcp", "0.0.0.0"), fixed(80, 80, "tcp", "192.168.1.10"), true},
		{"ipv6 wildcard vs ipv4 specific", fixed(80, 80, "tcp", "::"), fixed(80, 80, "tcp", "192.168.1.10"), true},
		{"different specific", fixed(80, 80, "tcp", "192.168.1.10"), fixed(80, 80, "tcp", "192.168.1.11"), false},
		{"same specific", fixed(80, 80, "tcp", "127.0.0.1"), fixed(80, 80, "tcp", "127.0.0.1"), true},
		{"ipv6 specific vs ipv4 specific", fixed(80, 80, "tcp", "::1"), fixed(80, 80, "tcp", "127.0.0.1"), false},
		{"block vs fixed udp inside", block(12000, 12004), fixed(12002, 12002, "udp", "10.0.0.1"), true},
		{"block vs fixed outside", block(12000, 12004), fixed(12005, 12005, "tcp", "0.0.0.0"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.Conflicts(c.b); got != c.want {
				t.Fatalf("Conflicts(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
			if got := c.b.Conflicts(c.a); got != c.want {
				t.Fatalf("Conflicts is not symmetric for %v / %v", c.a, c.b)
			}
		})
	}
}

func TestBindCovers(t *testing.T) {
	if !BindCovers("0.0.0.0", "127.0.0.1") || !BindCovers("::", "::1") {
		t.Fatal("wildcard reservation must cover specific binds")
	}
	if BindCovers("127.0.0.1", "0.0.0.0") {
		t.Fatal("specific reservation must not cover a wildcard bind")
	}
	if !BindCovers("127.0.0.1", "127.0.0.1") || BindCovers("127.0.0.1", "127.0.0.2") {
		t.Fatal("specific reservation covers only itself")
	}
}

func TestNormalizeBind(t *testing.T) {
	for in, want := range map[string]string{"": "0.0.0.0", "*": "0.0.0.0", "[::1]": "::1", "::ffff:10.0.0.1": "10.0.0.1", " 192.168.1.2 ": "192.168.1.2"} {
		got, err := NormalizeBind(in)
		if err != nil || got != want {
			t.Errorf("NormalizeBind(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"example.com", "300.1.1.1", "fe80::1%eth0"} {
		if _, err := NormalizeBind(bad); err == nil {
			t.Errorf("NormalizeBind(%q) succeeded", bad)
		}
	}
}

func TestFindBlockSequential(t *testing.T) {
	rng := Range{12000, 29999}
	var existing []Allocation
	for i := 0; i < 3; i++ {
		s, err := FindBlock(existing, rng, 5)
		if err != nil {
			t.Fatal(err)
		}
		if want := 12000 + 5*i; s != want {
			t.Fatalf("block %d starts at %d, want %d", i, s, want)
		}
		existing = append(existing, block(s, s+4))
	}
}

func TestFindBlockFirstFitGapAndSizeOverride(t *testing.T) {
	rng := Range{100, 200}
	existing := []Allocation{block(100, 104), block(110, 114), fixed(120, 120, "udp", "10.0.0.1")}
	s, err := FindBlock(existing, rng, 5)
	if err != nil || s != 105 {
		t.Fatalf("got %d, %v; want gap at 105", s, err)
	}
	s, err = FindBlock(existing, rng, 10)
	if err != nil || s != 121 {
		t.Fatalf("size 10: got %d, %v; want 121 (gap 105-109 too small, 115-119 too small)", s, err)
	}
	// Allocations outside the range are ignored.
	s, err = FindBlock([]Allocation{fixed(80, 80, "tcp", "0.0.0.0")}, rng, 5)
	if err != nil || s != 100 {
		t.Fatalf("got %d, %v", s, err)
	}
}

func TestFindBlockExhausted(t *testing.T) {
	rng := Range{100, 109}
	existing := []Allocation{block(100, 104), block(107, 109)}
	if _, err := FindBlock(existing, rng, 5); !errors.Is(err, ErrRangeExhausted) {
		t.Fatalf("expected exhaustion, got %v", err)
	}
	if s, err := FindBlock(existing, rng, 2); err != nil || s != 105 {
		t.Fatalf("got %d, %v", s, err)
	}
	if _, err := FindBlock(nil, rng, 11); err == nil {
		t.Fatal("block larger than range must fail")
	}
	if _, err := FindBlock(nil, rng, 0); err == nil {
		t.Fatal("zero block size must fail")
	}
}

func TestFindBlockTopOfPortSpace(t *testing.T) {
	rng := Range{65530, 65535}
	s, err := FindBlock(nil, rng, 6)
	if err != nil || s != 65530 {
		t.Fatalf("got %d, %v", s, err)
	}
	if _, err := FindBlock([]Allocation{block(65530, 65530)}, rng, 6); !errors.Is(err, ErrRangeExhausted) {
		t.Fatalf("expected exhaustion, got %v", err)
	}
	if _, err := FindBlock(nil, Range{65530, 65536}, 1); err == nil {
		t.Fatal("range beyond 65535 must be rejected")
	}
}

func TestParseFixedSpec(t *testing.T) {
	got, err := ParseFixedSpec("80, 81,443,53/udp,8000-8002/both", "tcp", "0.0.0.0", "web")
	if err != nil {
		t.Fatal(err)
	}
	want := []Allocation{
		{TypeFixed, 80, 80, "tcp", "0.0.0.0", "web"},
		{TypeFixed, 81, 81, "tcp", "0.0.0.0", "web"},
		{TypeFixed, 443, 443, "tcp", "0.0.0.0", "web"},
		{TypeFixed, 53, 53, "udp", "0.0.0.0", "web"},
		{TypeFixed, 8000, 8002, "both", "0.0.0.0", "web"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %v want %v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"0", "65536", "99999999999999999999", "80/sctp", "90-80", "abc", "", "-1"} {
		if _, err := ParseFixedSpec(bad, "tcp", "0.0.0.0", ""); err == nil {
			t.Errorf("ParseFixedSpec(%q) succeeded", bad)
		}
	}
}

func TestValidateStackName(t *testing.T) {
	for _, ok := range []string{"jellyfin", "nginx-proxy-manager", "a", "app_2"} {
		if err := ValidateStackName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../etc", "a/b", "Jellyfin", "-x", ".hidden", "list", "new", strings.Repeat("a", 64), "a b"} {
		if err := ValidateStackName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func validRegistry() *Registry {
	r := NewRegistry(time.Unix(0, 0).UTC())
	r.Stacks["a"] = &Stack{Name: "a", Directory: "/s/a", Allocations: []Allocation{fixed(80, 80, "tcp", "0.0.0.0"), block(12000, 12004)},
		Assignments: []Assignment{{Name: "web", HostPort: 80, ContainerPort: 80, Protocol: "tcp", BindAddress: "0.0.0.0", Status: AssignmentStatusAssigned}}}
	r.Stacks["b"] = &Stack{Name: "b", Directory: "/s/b", Allocations: []Allocation{fixed(53, 53, "udp", "0.0.0.0"), block(12005, 12009)}}
	return r
}

func TestRegistryValidate(t *testing.T) {
	if err := validRegistry().Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(r *Registry){
		"cross-stack overlap": func(r *Registry) { r.Stacks["b"].Allocations = append(r.Stacks["b"].Allocations, block(12003, 12007)) },
		"duplicate fixed": func(r *Registry) {
			r.Stacks["b"].Allocations = append(r.Stacks["b"].Allocations, fixed(80, 80, "tcp", "10.0.0.1"))
		},
		"self overlap": func(r *Registry) {
			r.Stacks["a"].Allocations = append(r.Stacks["a"].Allocations, fixed(12001, 12001, "tcp", "0.0.0.0"))
		},
		"assignment unreserved": func(r *Registry) { r.Stacks["a"].Assignments[0].HostPort = 81 },
		"assignment wrong proto": func(r *Registry) {
			r.Stacks["a"].Assignments[0].Protocol = "udp"
		},
		"duplicate assignment port": func(r *Registry) {
			r.Stacks["a"].Assignments = append(r.Stacks["a"].Assignments, Assignment{Name: "x", HostPort: 80, ContainerPort: 1, Protocol: "tcp", BindAddress: "0.0.0.0", Status: AssignmentStatusAssigned})
		},
		"bad version":     func(r *Registry) { r.Version = 2 },
		"key mismatch":    func(r *Registry) { r.Stacks["c"] = r.Stacks["a"] },
		"nil stack":       func(r *Registry) { r.Stacks["c"] = nil },
		"shared dir":      func(r *Registry) { r.Stacks["b"].Directory = "/s/a" },
		"port overflow":   func(r *Registry) { r.Stacks["b"].Allocations[1].End = 70000 },
		"inverted range":  func(r *Registry) { r.Stacks["b"].Allocations[1].End = 11000 },
		"block tcp only":  func(r *Registry) { r.Stacks["b"].Allocations[1].Protocol = "tcp" },
		"bad protocol":    func(r *Registry) { r.Stacks["b"].Allocations[0].Protocol = "UDP" },
		"bad bind":        func(r *Registry) { r.Stacks["b"].Allocations[0].BindAddress = "localhost" },
		"bad status":      func(r *Registry) { r.Stacks["a"].Assignments[0].Status = "active" },
		"bad stack name":  func(r *Registry) { r.Stacks["a"].Name = "A"; r.Stacks["A"] = r.Stacks["a"]; delete(r.Stacks, "a") },
		"unknown type":    func(r *Registry) { r.Stacks["b"].Allocations[0].Type = "dynamic" },
		"missing dir":     func(r *Registry) { r.Stacks["b"].Directory = "" },
		"zero start port": func(r *Registry) { r.Stacks["b"].Allocations[0].Start = 0 },
	}
	for name, mut := range mutations {
		t.Run(name, func(t *testing.T) {
			r := validRegistry()
			mut(r)
			if err := r.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSpecificBindsCoexist(t *testing.T) {
	r := validRegistry()
	r.Stacks["c"] = &Stack{Name: "c", Directory: "/s/c", Allocations: []Allocation{fixed(8080, 8080, "tcp", "192.168.1.10")}}
	r.Stacks["d"] = &Stack{Name: "d", Directory: "/s/d", Allocations: []Allocation{fixed(8080, 8080, "tcp", "192.168.1.11"), fixed(80, 80, "udp", "0.0.0.0")}}
	if err := r.Validate(); err != nil {
		t.Fatalf("distinct specific binds and tcp/udp on the same port must coexist: %v", err)
	}
	r.Stacks["e"] = &Stack{Name: "e", Directory: "/s/e", Allocations: []Allocation{fixed(8080, 8080, "tcp", "0.0.0.0")}}
	if err := r.Validate(); err == nil {
		t.Fatal("wildcard must conflict with specific binds")
	}
}

func TestDiffStackManifest(t *testing.T) {
	r := validRegistry()
	s := r.Stacks["a"]
	m := ManifestFor(s, nil)
	if d := DiffStackManifest(s, m); len(d) != 0 {
		t.Fatalf("unexpected diffs %v", d)
	}
	m.Allocations = m.Allocations[1:]
	m.Assignments[0].ContainerPort = 8080
	d := DiffStackManifest(s, m)
	if len(d) != 2 { // missing allocation + differing assignment
		t.Fatalf("diffs = %v", d)
	}
}
