package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/cmerk2021/ports/internal/compose"
	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

// StackInfo is the result of `ports <stack>`.
type StackInfo struct {
	Stack          *model.Stack       `json:"stack"`
	ManifestPath   string             `json:"manifest_path"`
	ManifestStatus string             `json:"manifest_status"`
	Discrepancies  []string           `json:"discrepancies"`
	ComposeStatus  string             `json:"compose_status"`
	ComposeError   string             `json:"compose_error,omitempty"`
	Mappings       []compose.Mapping  `json:"compose_mappings"`
	ComposeNotes   []compose.Note     `json:"compose_notes,omitempty"`
	Observed       *model.Observation `json:"last_recorded_observation,omitempty"`
}

// Manifest status values.
const (
	ManifestOK           = "ok"
	ManifestMissing      = "missing"
	ManifestDiffers      = "differs"
	ManifestUnreadable   = "unreadable"
	ComposeSkipped       = "skipped"
	ComposeStatusOK      = "ok"
	ComposeStatusInvalid = "invalid"
	ComposeStatusNoTool  = "unavailable"
)

// manifestState compares a stack's manifest with its registry entry.
func manifestState(s *model.Stack) (status string, diffs []string, m *model.Manifest) {
	m, err := store.ReadManifest(s.Directory)
	switch {
	case errors.Is(err, store.ErrManifestNotFound):
		return ManifestMissing, []string{fmt.Sprintf("manifest %s is missing", store.ManifestPath(s.Directory))}, nil
	case err != nil:
		return ManifestUnreadable, []string{err.Error()}, nil
	}
	diffs = model.DiffStackManifest(s, m)
	if len(diffs) > 0 {
		return ManifestDiffers, diffs, m
	}
	return ManifestOK, []string{}, m
}

// Lookup returns everything known about a stack. Compose inspection is
// optional and failures are reported, not fatal.
func (a *App) Lookup(ctx context.Context, name string, withCompose bool) (*StackInfo, error) {
	reg, err := a.readRegistry()
	if err != nil {
		return nil, err
	}
	s, err := lookupStack(reg, name)
	if err != nil {
		return nil, err
	}
	info := &StackInfo{Stack: s, ManifestPath: store.ManifestPath(s.Directory), Mappings: []compose.Mapping{}, ComposeStatus: ComposeSkipped}
	var m *model.Manifest
	info.ManifestStatus, info.Discrepancies, m = manifestState(s)
	if m != nil {
		info.Observed = m.Observed
	}
	if withCompose {
		files := s.ComposeFiles
		if len(files) == 0 {
			files = a.Cfg.ComposeFiles
		}
		res, err := compose.Inspect(ctx, a.Runner, s.Directory, files)
		var cfgErr *compose.ConfigError
		switch {
		case err == nil:
			info.ComposeStatus, info.Mappings, info.ComposeNotes = ComposeStatusOK, res.Mappings, res.Notes
		case errors.Is(err, compose.ErrUnavailable):
			info.ComposeStatus, info.ComposeError = ComposeStatusNoTool, err.Error()
		case errors.As(err, &cfgErr):
			info.ComposeStatus, info.ComposeError = ComposeStatusInvalid, cfgErr.Message
		default:
			info.ComposeStatus, info.ComposeError = ComposeStatusInvalid, err.Error()
		}
	}
	return info, nil
}

// List returns the registry.
func (a *App) List() (*model.Registry, error) {
	return a.readRegistry()
}

// FindMatch is one reservation containing the searched port.
type FindMatch struct {
	Stack       string             `json:"stack"`
	Directory   string             `json:"directory"`
	Allocation  model.Allocation   `json:"allocation"`
	Assignments []model.Assignment `json:"assignments"`
}

// Find returns reservations containing port, optionally filtered by protocol
// and bind address.
func (a *App) Find(port int, protocol, bind string) ([]FindMatch, error) {
	if err := model.ValidatePort(port); err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	if protocol != "" {
		p, err := model.NormalizeProtocol(protocol, "", true)
		if err != nil {
			return nil, errKind(ErrUsage, "%v", err)
		}
		protocol = p
	}
	if bind != "" {
		b, err := model.NormalizeBind(bind)
		if err != nil {
			return nil, errKind(ErrUsage, "%v", err)
		}
		bind = b
	}
	reg, err := a.readRegistry()
	if err != nil {
		return nil, err
	}
	out := []FindMatch{}
	for _, s := range reg.SortedStacks() {
		for _, al := range s.Allocations {
			if !al.Contains(port) {
				continue
			}
			if protocol != "" && !model.ProtocolsOverlap(al.Protocol, protocol) {
				continue
			}
			if bind != "" && !model.BindsOverlap(al.BindAddress, bind) {
				continue
			}
			fm := FindMatch{Stack: s.Name, Directory: s.Directory, Allocation: al, Assignments: []model.Assignment{}}
			for _, as := range s.Assignments {
				if as.HostPort == port && al.Covers(as.HostPort, as.Protocol, as.BindAddress) &&
					(protocol == "" || model.ProtocolsOverlap(as.Protocol, protocol)) {
					fm.Assignments = append(fm.Assignments, as)
				}
			}
			out = append(out, fm)
		}
	}
	if len(out) == 0 {
		return out, errKind(ErrNotFound, "no stack reserves port %d%s (this does not mean the port is free on the host)", port, protoSuffix(protocol))
	}
	return out, nil
}

func protoSuffix(p string) string {
	if p == "" {
		return ""
	}
	return "/" + p
}
