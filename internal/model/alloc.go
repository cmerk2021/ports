package model

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrRangeExhausted is returned when no free block fits in the dynamic range.
var ErrRangeExhausted = errors.New("dynamic allocation range exhausted")

// Range is an inclusive port range.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Validate checks that the range lies within 1-65535 and is ordered.
func (r Range) Validate() error {
	if err := ValidatePort(r.Start); err != nil {
		return fmt.Errorf("range start: %w", err)
	}
	if err := ValidatePort(r.End); err != nil {
		return fmt.Errorf("range end: %w", err)
	}
	if r.End < r.Start {
		return fmt.Errorf("range %d-%d: end is before start", r.Start, r.End)
	}
	return nil
}

// Size returns the number of ports in the range.
func (r Range) Size() int { return r.End - r.Start + 1 }

// FindBlock returns the lowest start port s such that [s, s+size-1] lies within
// rng and does not overlap any of the given allocations (sequential first-fit).
// Dynamic blocks reserve TCP+UDP on all addresses, so any overlapping
// allocation, whatever its protocol or bind address, makes a candidate
// unusable.
func FindBlock(existing []Allocation, rng Range, size int) (int, error) {
	if err := rng.Validate(); err != nil {
		return 0, fmt.Errorf("invalid dynamic range: %w", err)
	}
	if size < 1 {
		return 0, fmt.Errorf("invalid block size %d: must be at least 1", size)
	}
	if size > rng.Size() {
		return 0, fmt.Errorf("block size %d is larger than the dynamic range %d-%d (%d ports)", size, rng.Start, rng.End, rng.Size())
	}
	var busy []Allocation
	for _, a := range existing {
		if a.End >= rng.Start && a.Start <= rng.End {
			busy = append(busy, a)
		}
	}
	sort.Slice(busy, func(i, j int) bool { return busy[i].Start < busy[j].Start })
	cur := rng.Start
	for _, a := range busy {
		if a.Start > cur && a.Start-cur >= size {
			return cur, nil
		}
		if a.End+1 > cur {
			cur = a.End + 1
		}
		if cur > rng.End {
			break
		}
	}
	if cur <= rng.End && rng.End-cur+1 >= size {
		return cur, nil
	}
	return 0, fmt.Errorf("%w: no free %d-port block in %d-%d", ErrRangeExhausted, size, rng.Start, rng.End)
}

// AllAllocations returns every allocation in the registry.
func (r *Registry) AllAllocations() []Allocation {
	var out []Allocation
	for _, s := range r.SortedStacks() {
		out = append(out, s.Allocations...)
	}
	return out
}

// ParsePortRange parses "80" or "8000-8005".
func ParsePortRange(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	lo, hi, isRange := strings.Cut(s, "-")
	start, err := parsePort(lo)
	if err != nil {
		return 0, 0, err
	}
	end := start
	if isRange {
		if end, err = parsePort(hi); err != nil {
			return 0, 0, err
		}
		if end < start {
			return 0, 0, fmt.Errorf("invalid port range %q: end is before start", s)
		}
	}
	return start, end, nil
}

// ParsePort parses a single port number.
func ParsePort(s string) (int, error) { return parsePort(s) }

func parsePort(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("missing port number")
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: expected a number between %d and %d", s, MinPort, MaxPort)
	}
	if err := ValidatePort(int(n)); err != nil {
		return 0, err
	}
	return int(n), nil
}

// ParseFixedSpec parses a comma-separated list of fixed reservations such as
// "80,81,443", "8000-8005" or "53/udp,53/tcp". Entries without an explicit
// protocol use defProto; all entries use bind.
func ParseFixedSpec(spec, defProto, bind, purpose string) ([]Allocation, error) {
	var out []Allocation
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		portPart, protoPart, _ := strings.Cut(item, "/")
		proto, err := NormalizeProtocol(protoPart, defProto, true)
		if err != nil {
			return nil, fmt.Errorf("fixed port %q: %w", item, err)
		}
		start, end, err := ParsePortRange(portPart)
		if err != nil {
			return nil, fmt.Errorf("fixed port %q: %w", item, err)
		}
		out = append(out, Allocation{Type: TypeFixed, Start: start, End: end, Protocol: proto, BindAddress: bind, Purpose: purpose})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no fixed ports given in %q", spec)
	}
	return out, nil
}
