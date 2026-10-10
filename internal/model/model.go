// Package model defines the port-allocation data model shared by the global
// registry and the per-stack manifests, together with validation and
// conflict-detection rules.
package model

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the only registry/manifest schema version understood by
// this binary. Files with any other version are never modified.
const SchemaVersion = 1

// Port bounds.
const (
	MinPort = 1
	MaxPort = 65535
)

// Allocation types.
const (
	TypeFixed = "fixed"
	TypeBlock = "block"
)

// Protocols. ProtoBoth reserves the port(s) for TCP and UDP simultaneously.
const (
	ProtoTCP  = "tcp"
	ProtoUDP  = "udp"
	ProtoBoth = "both"
)

// WildcardIPv4 is the default bind address. Both 0.0.0.0 and :: are treated as
// wildcards that may conflict with every address (see BindsOverlap).
const WildcardIPv4 = "0.0.0.0"

// AssignmentStatusAssigned marks an intended (not necessarily published)
// assignment. Observed Compose state is never stored as an assignment status.
const AssignmentStatusAssigned = "assigned"

// Allocation is a reservation of one or more consecutive host ports.
type Allocation struct {
	Type        string `json:"type"`
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Protocol    string `json:"protocol"`
	BindAddress string `json:"bind_address"`
	Purpose     string `json:"purpose,omitempty"`
}

// Assignment maps a reserved host port to a container port.
type Assignment struct {
	Name          string `json:"name"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
	BindAddress   string `json:"bind_address"`
	Purpose       string `json:"purpose,omitempty"`
	Status        string `json:"status"`
}

// Stack is the registry record for one stack.
type Stack struct {
	Name         string       `json:"name"`
	Directory    string       `json:"directory"`
	ComposeFiles []string     `json:"compose_files,omitempty"`
	Allocations  []Allocation `json:"allocations"`
	Assignments  []Assignment `json:"assignments"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// Registry is the authoritative global record.
type Registry struct {
	Version   int               `json:"version"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Stacks    map[string]*Stack `json:"stacks"`
}

// ObservedMapping is a published port observed in the effective Compose
// configuration during an audit. It is informational only.
type ObservedMapping struct {
	Service       string `json:"service"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
	BindAddress   string `json:"bind_address"`
}

// Observation records the result of the last recorded audit.
type Observation struct {
	CheckedAt time.Time         `json:"checked_at"`
	Mappings  []ObservedMapping `json:"mappings"`
}

// Manifest is the local .ports file stored in each stack directory. It mirrors
// the registry entry and may additionally hold the last observed Compose
// mappings, which are kept strictly separate from reservations and intended
// assignments.
type Manifest struct {
	Version      int          `json:"version"`
	Stack        string       `json:"stack"`
	Directory    string       `json:"directory"`
	ComposeFiles []string     `json:"compose_files,omitempty"`
	Allocations  []Allocation `json:"allocations"`
	Assignments  []Assignment `json:"assignments"`
	UpdatedAt    time.Time    `json:"updated_at"`
	Observed     *Observation `json:"observed,omitempty"`
}

// NewRegistry returns an empty registry.
func NewRegistry(now time.Time) *Registry {
	return &Registry{Version: SchemaVersion, CreatedAt: now, UpdatedAt: now, Stacks: map[string]*Stack{}}
}

// Clone returns a deep copy of the registry.
func (r *Registry) Clone() *Registry {
	c := *r
	c.Stacks = make(map[string]*Stack, len(r.Stacks))
	for k, s := range r.Stacks {
		c.Stacks[k] = s.Clone()
	}
	return &c
}

// SortedStacks returns stacks ordered by name.
func (r *Registry) SortedStacks() []*Stack {
	out := make([]*Stack, 0, len(r.Stacks))
	for _, s := range r.Stacks {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Clone returns a deep copy of the stack.
func (s *Stack) Clone() *Stack {
	c := *s
	c.ComposeFiles = append([]string(nil), s.ComposeFiles...)
	c.Allocations = append([]Allocation{}, s.Allocations...)
	c.Assignments = append([]Assignment{}, s.Assignments...)
	return &c
}

// Normalize sorts allocations and assignments into canonical order.
func (s *Stack) Normalize() {
	SortAllocations(s.Allocations)
	sort.Slice(s.Assignments, func(i, j int) bool { return s.Assignments[i].Name < s.Assignments[j].Name })
	if s.Allocations == nil {
		s.Allocations = []Allocation{}
	}
	if s.Assignments == nil {
		s.Assignments = []Assignment{}
	}
}

// ManifestFor builds the manifest corresponding to a registry entry. The
// observation (if any) is carried over from prev.
func ManifestFor(s *Stack, prev *Manifest) *Manifest {
	c := s.Clone()
	c.Normalize()
	m := &Manifest{
		Version:      SchemaVersion,
		Stack:        c.Name,
		Directory:    c.Directory,
		ComposeFiles: c.ComposeFiles,
		Allocations:  c.Allocations,
		Assignments:  c.Assignments,
		UpdatedAt:    s.UpdatedAt,
	}
	if prev != nil {
		m.Observed = prev.Observed
	}
	return m
}

// StackFromManifest builds a registry entry from a manifest (used for
// recovery).
func StackFromManifest(m *Manifest, now time.Time) *Stack {
	s := &Stack{
		Name:         m.Stack,
		Directory:    m.Directory,
		ComposeFiles: append([]string(nil), m.ComposeFiles...),
		Allocations:  append([]Allocation{}, m.Allocations...),
		Assignments:  append([]Assignment{}, m.Assignments...),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if !m.UpdatedAt.IsZero() {
		s.UpdatedAt = m.UpdatedAt
	}
	s.Normalize()
	return s
}

// SortAllocations sorts allocations by start port, then type and protocol.
func SortAllocations(a []Allocation) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Start != a[j].Start {
			return a[i].Start < a[j].Start
		}
		if a[i].Type != a[j].Type {
			return a[i].Type > a[j].Type // fixed before block
		}
		return a[i].Protocol < a[j].Protocol
	})
}

// --- names -----------------------------------------------------------------

var stackNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var assignmentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// ReservedNames cannot be used as stack names because they collide with CLI
// subcommands (`ports <stack>` lookup).
var ReservedNames = map[string]bool{
	"new": true, "list": true, "ls": true, "find": true, "assign": true, "reserve": true,
	"release": true, "check": true, "audit": true, "sync": true, "delete": true,
	"show": true, "init": true, "help": true, "version": true, "add-block": true,
}

// ValidateStackName checks that name is a safe, Compose-compatible directory
// name. The pattern excludes path separators, "." and "..".
func ValidateStackName(name string) error {
	if !stackNameRe.MatchString(name) {
		return fmt.Errorf("invalid stack name %q: use 1-63 lowercase letters, digits, '-' or '_', starting with a letter or digit", name)
	}
	if ReservedNames[name] {
		return fmt.Errorf("invalid stack name %q: it is a reserved ports subcommand name", name)
	}
	return nil
}

// ValidateAssignmentName checks an assignment name.
func ValidateAssignmentName(name string) error {
	if !assignmentNameRe.MatchString(name) {
		return fmt.Errorf("invalid assignment name %q: use 1-63 letters, digits, '.', '-' or '_', starting with a letter or digit", name)
	}
	return nil
}

// --- ports, protocols and bind addresses ------------------------------------

// ValidatePort checks that p is within 1-65535.
func ValidatePort(p int) error {
	if p < MinPort || p > MaxPort {
		return fmt.Errorf("port %d is out of range (valid ports are %d-%d)", p, MinPort, MaxPort)
	}
	return nil
}

// NormalizeProtocol lowercases and validates a protocol. Empty means def.
func NormalizeProtocol(p, def string, allowBoth bool) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		p = def
	}
	switch p {
	case ProtoTCP, ProtoUDP:
		return p, nil
	case ProtoBoth:
		if allowBoth {
			return p, nil
		}
	}
	if allowBoth {
		return "", fmt.Errorf("invalid protocol %q: expected tcp, udp or both", p)
	}
	return "", fmt.Errorf("invalid protocol %q: expected tcp or udp", p)
}

// NormalizeBind validates and canonicalizes a bind address. Empty means the
// IPv4 wildcard. Brackets around IPv6 addresses are accepted.
func NormalizeBind(b string) (string, error) {
	b = strings.TrimSpace(b)
	b = strings.TrimSuffix(strings.TrimPrefix(b, "["), "]")
	if b == "" || b == "*" {
		return WildcardIPv4, nil
	}
	a, err := netip.ParseAddr(b)
	if err != nil {
		return "", fmt.Errorf("invalid bind address %q: expected an IPv4 or IPv6 address", b)
	}
	if a.Zone() != "" {
		return "", fmt.Errorf("invalid bind address %q: zoned IPv6 addresses are not supported", b)
	}
	return a.Unmap().String(), nil
}

// IsWildcard reports whether b is an unspecified (wildcard) address.
func IsWildcard(b string) bool {
	if b == "" || b == "*" {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(b, "[]"))
	return err == nil && a.IsUnspecified()
}

// BindsOverlap reports whether two bind addresses could contend for the same
// socket. Wildcards are treated conservatively: 0.0.0.0 and :: overlap every
// address of either family (Docker publishes wildcard ports on both families,
// and dual-stack sockets are common).
func BindsOverlap(a, b string) bool {
	if IsWildcard(a) || IsWildcard(b) {
		return true
	}
	na, errA := NormalizeBind(a)
	nb, errB := NormalizeBind(b)
	if errA != nil || errB != nil {
		return true // unknown: be conservative
	}
	return na == nb
}

// BindCovers reports whether a reservation bound to resv covers a socket bound
// to use. A wildcard reservation covers everything; a specific reservation
// only covers the identical address (a wildcard bind is broader than it).
func BindCovers(resv, use string) bool {
	if IsWildcard(resv) {
		return true
	}
	if IsWildcard(use) {
		return false
	}
	nr, err1 := NormalizeBind(resv)
	nu, err2 := NormalizeBind(use)
	return err1 == nil && err2 == nil && nr == nu
}

// ProtocolsOverlap reports whether two protocols overlap ("both" overlaps all).
func ProtocolsOverlap(a, b string) bool {
	return a == b || a == ProtoBoth || b == ProtoBoth
}

// ProtocolCovers reports whether reservation protocol resv covers use.
func ProtocolCovers(resv, use string) bool {
	return resv == ProtoBoth || resv == use
}

// --- allocations ------------------------------------------------------------

// Size returns the number of ports in the allocation.
func (a Allocation) Size() int { return a.End - a.Start + 1 }

// Contains reports whether port p lies within the allocation range.
func (a Allocation) Contains(p int) bool { return p >= a.Start && p <= a.End }

// Covers reports whether the allocation reserves host port p for the given
// protocol and bind address.
func (a Allocation) Covers(p int, proto, bind string) bool {
	return a.Contains(p) && ProtocolCovers(a.Protocol, proto) && BindCovers(a.BindAddress, bind)
}

// RangeString formats the port range.
func (a Allocation) RangeString() string {
	if a.Start == a.End {
		return fmt.Sprintf("%d", a.Start)
	}
	return fmt.Sprintf("%d-%d", a.Start, a.End)
}

// String formats the allocation for humans.
func (a Allocation) String() string {
	s := fmt.Sprintf("%s %s/%s on %s", a.Type, a.RangeString(), a.Protocol, a.BindAddress)
	if a.Purpose != "" {
		s += fmt.Sprintf(" (%s)", a.Purpose)
	}
	return s
}

// Conflicts reports whether two allocations overlap in port range, protocol
// and bind scope.
func (a Allocation) Conflicts(b Allocation) bool {
	return a.Start <= b.End && b.Start <= a.End &&
		ProtocolsOverlap(a.Protocol, b.Protocol) && BindsOverlap(a.BindAddress, b.BindAddress)
}

// Validate checks a single allocation.
func (a Allocation) Validate() error {
	switch a.Type {
	case TypeFixed, TypeBlock:
	default:
		return fmt.Errorf("invalid allocation type %q: expected %q or %q", a.Type, TypeFixed, TypeBlock)
	}
	if err := ValidatePort(a.Start); err != nil {
		return fmt.Errorf("allocation start: %w", err)
	}
	if err := ValidatePort(a.End); err != nil {
		return fmt.Errorf("allocation end: %w", err)
	}
	if a.End < a.Start {
		return fmt.Errorf("allocation %d-%d: end is before start", a.Start, a.End)
	}
	if p, err := NormalizeProtocol(a.Protocol, "", true); err != nil || p != a.Protocol {
		return fmt.Errorf("allocation %s: invalid protocol %q (expected tcp, udp or both)", a.RangeString(), a.Protocol)
	}
	nb, err := NormalizeBind(a.BindAddress)
	if err != nil || nb != a.BindAddress {
		return fmt.Errorf("allocation %s: invalid or non-canonical bind address %q", a.RangeString(), a.BindAddress)
	}
	if a.Type == TypeBlock && (a.Protocol != ProtoBoth || !IsWildcard(a.BindAddress)) {
		return fmt.Errorf("block allocation %s must reserve protocol %q on a wildcard address", a.RangeString(), ProtoBoth)
	}
	return nil
}

// Validate checks a single assignment.
func (as Assignment) Validate() error {
	if err := ValidateAssignmentName(as.Name); err != nil {
		return err
	}
	if err := ValidatePort(as.HostPort); err != nil {
		return fmt.Errorf("assignment %q host port: %w", as.Name, err)
	}
	if err := ValidatePort(as.ContainerPort); err != nil {
		return fmt.Errorf("assignment %q container port: %w", as.Name, err)
	}
	if as.Protocol != ProtoTCP && as.Protocol != ProtoUDP {
		return fmt.Errorf("assignment %q: invalid protocol %q (expected tcp or udp)", as.Name, as.Protocol)
	}
	nb, err := NormalizeBind(as.BindAddress)
	if err != nil || nb != as.BindAddress {
		return fmt.Errorf("assignment %q: invalid or non-canonical bind address %q", as.Name, as.BindAddress)
	}
	if as.Status != AssignmentStatusAssigned {
		return fmt.Errorf("assignment %q: invalid status %q (expected %q)", as.Name, as.Status, AssignmentStatusAssigned)
	}
	return nil
}

// String formats an assignment for humans.
func (as Assignment) String() string {
	s := fmt.Sprintf("%s: %s:%d -> %d/%s", as.Name, as.BindAddress, as.HostPort, as.ContainerPort, as.Protocol)
	if as.Purpose != "" {
		s += fmt.Sprintf(" (%s)", as.Purpose)
	}
	return s
}

// Covering returns the allocation of s that reserves port p for proto/bind.
func (s *Stack) Covering(p int, proto, bind string) (Allocation, bool) {
	for _, a := range s.Allocations {
		if a.Covers(p, proto, bind) {
			return a, true
		}
	}
	return Allocation{}, false
}

// FindAssignment returns the index of the named assignment or -1.
func (s *Stack) FindAssignment(name string) int {
	for i, a := range s.Assignments {
		if a.Name == name {
			return i
		}
	}
	return -1
}

// Validate checks a stack record in isolation.
func (s *Stack) Validate() error {
	if err := ValidateStackName(s.Name); err != nil {
		return err
	}
	if s.Directory == "" {
		return fmt.Errorf("stack %q: missing directory", s.Name)
	}
	for i, a := range s.Allocations {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("stack %q: %w", s.Name, err)
		}
		for _, b := range s.Allocations[i+1:] {
			if a.Conflicts(b) {
				return fmt.Errorf("stack %q: allocations %s and %s overlap", s.Name, a, b)
			}
		}
	}
	names := map[string]bool{}
	for i, as := range s.Assignments {
		if err := as.Validate(); err != nil {
			return fmt.Errorf("stack %q: %w", s.Name, err)
		}
		if names[as.Name] {
			return fmt.Errorf("stack %q: duplicate assignment name %q", s.Name, as.Name)
		}
		names[as.Name] = true
		if _, ok := s.Covering(as.HostPort, as.Protocol, as.BindAddress); !ok {
			return fmt.Errorf("stack %q: assignment %q uses %s:%d/%s which is not reserved by the stack", s.Name, as.Name, as.BindAddress, as.HostPort, as.Protocol)
		}
		for _, o := range s.Assignments[i+1:] {
			if o.HostPort == as.HostPort && o.Protocol == as.Protocol && BindsOverlap(o.BindAddress, as.BindAddress) {
				return fmt.Errorf("stack %q: assignments %q and %q both use host port %d/%s", s.Name, as.Name, o.Name, as.HostPort, as.Protocol)
			}
		}
	}
	return nil
}

// Conflict describes two overlapping allocations owned by different stacks.
type Conflict struct {
	StackA, StackB string
	A, B           Allocation
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s [%s] overlaps %s [%s]", c.StackA, c.A, c.StackB, c.B)
}

// CrossStackConflicts returns every pair of overlapping allocations owned by
// different stacks.
func (r *Registry) CrossStackConflicts() []Conflict {
	var out []Conflict
	stacks := r.SortedStacks()
	for i, s := range stacks {
		for _, t := range stacks[i+1:] {
			for _, a := range s.Allocations {
				for _, b := range t.Allocations {
					if a.Conflicts(b) {
						out = append(out, Conflict{s.Name, t.Name, a, b})
					}
				}
			}
		}
	}
	return out
}

// Validate checks the complete registry: schema, every stack and the absence
// of cross-stack overlaps. It must succeed before any registry is committed.
func (r *Registry) Validate() error {
	if r.Version != SchemaVersion {
		return fmt.Errorf("unsupported registry schema version %d (this binary supports %d)", r.Version, SchemaVersion)
	}
	for key, s := range r.Stacks {
		if s == nil {
			return fmt.Errorf("registry entry %q is empty", key)
		}
		if key != s.Name {
			return fmt.Errorf("registry key %q does not match stack name %q", key, s.Name)
		}
	}
	dirs := map[string]string{}
	for _, s := range r.SortedStacks() {
		if err := s.Validate(); err != nil {
			return err
		}
		if other, ok := dirs[s.Directory]; ok {
			return fmt.Errorf("stacks %q and %q share directory %s", other, s.Name, s.Directory)
		}
		dirs[s.Directory] = s.Name
	}
	if c := r.CrossStackConflicts(); len(c) > 0 {
		return fmt.Errorf("conflicting reservations: %s", c[0])
	}
	return nil
}

// ConflictsWith returns allocations in r (excluding stack skip) that conflict
// with a.
func (r *Registry) ConflictsWith(a Allocation, skip string) []Conflict {
	var out []Conflict
	for _, s := range r.SortedStacks() {
		if s.Name == skip {
			continue
		}
		for _, b := range s.Allocations {
			if a.Conflicts(b) {
				out = append(out, Conflict{StackA: skip, StackB: s.Name, A: a, B: b})
			}
		}
	}
	return out
}

// Validate checks a manifest in isolation.
func (m *Manifest) Validate() error {
	if m.Version != SchemaVersion {
		return fmt.Errorf("unsupported manifest schema version %d (this binary supports %d)", m.Version, SchemaVersion)
	}
	s := &Stack{Name: m.Stack, Directory: m.Directory, Allocations: m.Allocations, Assignments: m.Assignments}
	return s.Validate()
}
