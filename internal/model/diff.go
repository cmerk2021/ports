package model

import (
	"fmt"
	"reflect"
	"strings"
)

// DiffStackManifest lists differences between a registry entry and a local
// manifest. Timestamps and observed audit data are ignored.
func DiffStackManifest(s *Stack, m *Manifest) []string {
	var out []string
	if s.Name != m.Stack {
		out = append(out, fmt.Sprintf("stack name: registry %q, manifest %q", s.Name, m.Stack))
	}
	if s.Directory != m.Directory {
		out = append(out, fmt.Sprintf("directory: registry %q, manifest %q", s.Directory, m.Directory))
	}
	if strings.Join(s.ComposeFiles, ",") != strings.Join(m.ComposeFiles, ",") {
		out = append(out, fmt.Sprintf("compose files: registry %v, manifest %v", s.ComposeFiles, m.ComposeFiles))
	}
	a, b := s.Clone(), StackFromManifest(m, s.UpdatedAt)
	a.Normalize()
	for _, x := range a.Allocations {
		if !containsAlloc(b.Allocations, x) {
			out = append(out, fmt.Sprintf("allocation %s is in the registry but not in the manifest", x))
		}
	}
	for _, x := range b.Allocations {
		if !containsAlloc(a.Allocations, x) {
			out = append(out, fmt.Sprintf("allocation %s is in the manifest but not in the registry", x))
		}
	}
	for _, x := range a.Assignments {
		i := b.FindAssignment(x.Name)
		switch {
		case i < 0:
			out = append(out, fmt.Sprintf("assignment %q is in the registry but not in the manifest", x.Name))
		case !reflect.DeepEqual(x, b.Assignments[i]):
			out = append(out, fmt.Sprintf("assignment %q differs: registry {%s}, manifest {%s}", x.Name, x, b.Assignments[i]))
		}
	}
	for _, x := range b.Assignments {
		if a.FindAssignment(x.Name) < 0 {
			out = append(out, fmt.Sprintf("assignment %q is in the manifest but not in the registry", x.Name))
		}
	}
	return out
}

func containsAlloc(list []Allocation, a Allocation) bool {
	for _, b := range list {
		if a == b {
			return true
		}
	}
	return false
}
