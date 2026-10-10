package app

import (
	"context"
	"fmt"
	"os"

	"github.com/cmerk2021/ports/internal/audit"
	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

// AuditOptions configures `ports check` / `ports audit`.
type AuditOptions struct {
	Stacks       []string // empty means all registered stacks
	ComposeFiles []string // override compose files (single-stack check only)
	Host         bool     // inspect host listeners
	Record       bool     // store observed mappings in the manifests
}

// Audit checks stacks against their effective Compose configuration and,
// optionally, host listeners. The registry lock is not held while Docker
// runs.
func (a *App) Audit(ctx context.Context, opt AuditOptions) (*audit.Report, error) {
	reg, err := a.readRegistry()
	if err != nil {
		return nil, err
	}
	var stacks []*model.Stack
	if len(opt.Stacks) == 0 {
		stacks = reg.SortedStacks()
	} else {
		for _, n := range opt.Stacks {
			s, err := lookupStack(reg, n)
			if err != nil {
				return nil, err
			}
			stacks = append(stacks, s)
		}
	}
	rep := &audit.Report{Stacks: []audit.StackReport{}, HostScan: "disabled"}
	aopt := audit.Options{Runner: a.Runner, ComposeFiles: a.Cfg.ComposeFiles}
	if opt.Host {
		if snap := a.host(); snap != nil {
			rep.HostScan = "ok"
			for _, u := range snap.Unavailable {
				rep.Global = append(rep.Global, audit.Finding{Severity: audit.SevInfo, Code: audit.CodeHostUnavailable, Message: "host socket inspection incomplete: " + u})
			}
			if !snap.Available() {
				rep.HostScan = "unavailable"
			} else {
				aopt.Host = snap
				if len(opt.Stacks) == 0 {
					rep.Global = append(rep.Global, audit.UnreservedListeners(reg, a.Cfg.DynamicRange, snap)...)
				}
			}
		}
	}
	for _, c := range reg.CrossStackConflicts() {
		rep.Global = append(rep.Global, audit.Finding{Severity: audit.SevViolation, Code: audit.CodeRegistryConflict, Message: c.String()})
	}

	for _, s := range stacks {
		var pre []audit.Finding
		if fi, err := os.Lstat(s.Directory); err != nil || !fi.IsDir() {
			sr := audit.StackReport{Stack: s.Name, Directory: s.Directory, ComposeStatus: audit.ComposeInvalid, Mappings: nil,
				Findings: []audit.Finding{{Stack: s.Name, Severity: audit.SevError, Code: audit.CodeManifestProblem,
					Message: fmt.Sprintf("stack directory %s is missing or not a directory", s.Directory)}}}
			rep.Stacks = append(rep.Stacks, sr)
			continue
		}
		status, diffs, _ := manifestState(s)
		switch status {
		case ManifestDiffers:
			for _, d := range diffs {
				pre = append(pre, audit.Finding{Stack: s.Name, Severity: audit.SevViolation, Code: audit.CodeManifestMismatch, Message: d + " (run `ports sync --dry-run`)"})
			}
		case ManifestMissing, ManifestUnreadable:
			for _, d := range diffs {
				pre = append(pre, audit.Finding{Stack: s.Name, Severity: audit.SevViolation, Code: audit.CodeManifestProblem, Message: d + " (run `ports sync --dry-run`)"})
			}
		}
		check := s
		if len(opt.ComposeFiles) > 0 {
			check = s.Clone()
			check.ComposeFiles = opt.ComposeFiles
		}
		sr := audit.CheckStack(ctx, reg, check, aopt)
		sr.Findings = append(pre, sr.Findings...)
		rep.Stacks = append(rep.Stacks, sr)
	}
	rep.Finalize()

	if opt.Record {
		if err := a.recordObservations(rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// recordObservations stores observed Compose mappings in consistent
// manifests. Only the manifest's "observed" section changes.
func (a *App) recordObservations(rep *audit.Report) error {
	return a.withRegistry(func(reg *model.Registry) error {
		now := a.Now()
		for _, sr := range rep.Stacks {
			if sr.ComposeStatus != audit.ComposeOK {
				continue
			}
			s, ok := reg.Stacks[sr.Stack]
			if !ok {
				continue
			}
			m, err := store.ReadManifest(s.Directory)
			if err != nil || len(model.DiffStackManifest(s, m)) > 0 {
				continue
			}
			obs := &model.Observation{CheckedAt: now, Mappings: []model.ObservedMapping{}}
			for _, mp := range sr.Mappings {
				obs.Mappings = append(obs.Mappings, model.ObservedMapping{Service: mp.Service, HostPort: mp.HostPort, ContainerPort: mp.ContainerPort, Protocol: mp.Protocol, BindAddress: mp.BindAddress})
			}
			m.Observed = obs
			if err := store.WriteManifest(s.Directory, m, false); err != nil {
				return fmt.Errorf("cannot record observations for %s: %w", s.Name, err)
			}
		}
		return nil
	})
}
