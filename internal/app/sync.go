package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

// Sync item kinds.
const (
	SyncManifestMissing     = "manifest-missing"
	SyncManifestDiffers     = "manifest-differs"
	SyncManifestCorrupt     = "manifest-corrupt"
	SyncManifestUnsupported = "manifest-unsupported-version"
	SyncManifestUnsafe      = "manifest-unsafe"
	SyncDirectoryMissing    = "directory-missing"
	SyncDirectoryUnsafe     = "directory-unsafe"
	SyncOrphanManifest      = "unregistered-manifest"
)

// SyncItem is one discrepancy between the registry and the manifests.
type SyncItem struct {
	Stack   string   `json:"stack"`
	Kind    string   `json:"kind"`
	Details []string `json:"details"`
	Action  string   `json:"proposed_action"`
	Safe    bool     `json:"safe_to_apply"`
	Applied bool     `json:"applied"`
	Result  string   `json:"result,omitempty"`

	stack    *model.Stack    // registry entry to write a manifest for
	manifest *model.Manifest // manifest to import
}

// SyncReport is the outcome of `ports sync`.
type SyncReport struct {
	DryRun bool       `json:"dry_run"`
	Items  []SyncItem `json:"items"`
}

// Pending reports whether discrepancies remain unresolved.
func (r *SyncReport) Pending() bool {
	for _, it := range r.Items {
		if !it.Applied {
			return true
		}
	}
	return false
}

// Sync compares the registry with every manifest. With apply set, safe and
// unambiguous repairs are performed:
//   - a missing, corrupt or differing manifest is rewritten from the
//     authoritative registry (any existing file is first preserved as
//     ".ports.bak-<timestamp>");
//   - a valid manifest found under the stack root that is not registered and
//     does not conflict with the registry is imported.
//
// Anything else (missing directories, symlinks, unsupported schema versions,
// conflicting manifests) is only reported.
func (a *App) Sync(apply bool) (*SyncReport, error) {
	rep := &SyncReport{DryRun: !apply, Items: []SyncItem{}}
	run := func(reg *model.Registry) error {
		rep.Items = a.planSync(reg)
		if apply {
			return a.applySync(reg, rep)
		}
		return nil
	}
	var err error
	if apply {
		err = a.withRegistry(run)
	} else {
		var reg *model.Registry
		if reg, err = a.readRegistry(); err == nil {
			err = run(reg)
		}
	}
	if err != nil {
		return nil, err
	}
	return rep, nil
}

func (a *App) planSync(reg *model.Registry) []SyncItem {
	items := []SyncItem{}
	for _, s := range reg.SortedStacks() {
		fi, err := os.Lstat(s.Directory)
		switch {
		case errors.Is(err, os.ErrNotExist):
			items = append(items, SyncItem{Stack: s.Name, Kind: SyncDirectoryMissing,
				Details: []string{fmt.Sprintf("stack directory %s does not exist", s.Directory)},
				Action:  fmt.Sprintf("none (recreate the directory, or remove the registration with `ports delete %s --release-ports`)", s.Name)})
			continue
		case err != nil:
			items = append(items, SyncItem{Stack: s.Name, Kind: SyncDirectoryUnsafe, Details: []string{err.Error()}, Action: "none (manual review required)"})
			continue
		case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
			items = append(items, SyncItem{Stack: s.Name, Kind: SyncDirectoryUnsafe,
				Details: []string{fmt.Sprintf("%s is a symlink or not a directory", s.Directory)}, Action: "none (manual review required)"})
			continue
		}
		m, err := store.ReadManifest(s.Directory)
		switch {
		case errors.Is(err, store.ErrManifestNotFound):
			items = append(items, SyncItem{Stack: s.Name, Kind: SyncManifestMissing, Details: []string{err.Error()},
				Action: "write the manifest from the registry", Safe: true, stack: s})
		case errors.Is(err, store.ErrUnsupported):
			items = append(items, SyncItem{Stack: s.Name, Kind: SyncManifestUnsupported, Details: []string{err.Error()},
				Action: "none (written by a newer version of ports; upgrade or review manually)"})
		case errors.Is(err, store.ErrUnsafePath):
			items = append(items, SyncItem{Stack: s.Name, Kind: SyncManifestUnsafe, Details: []string{err.Error()},
				Action: "none (remove the symlink or special file manually)"})
		case err != nil:
			items = append(items, SyncItem{Stack: s.Name, Kind: SyncManifestCorrupt, Details: []string{err.Error()},
				Action: "preserve the corrupt file as .ports.bak-<time> and rewrite it from the registry", Safe: true, stack: s})
		default:
			if diffs := model.DiffStackManifest(s, m); len(diffs) > 0 {
				items = append(items, SyncItem{Stack: s.Name, Kind: SyncManifestDiffers, Details: diffs,
					Action: "preserve the manifest as .ports.bak-<time> and rewrite it from the (authoritative) registry", Safe: true, stack: s})
			}
		}
	}

	names, err := store.ScanManifests(a.Cfg.StackRoot)
	if err != nil {
		items = append(items, SyncItem{Kind: SyncDirectoryUnsafe, Details: []string{err.Error()}, Action: "none (unregistered manifests were not scanned)"})
		return items
	}
	proposed := reg.Clone()
	for _, name := range names {
		if _, ok := reg.Stacks[name]; ok {
			continue
		}
		dir := filepath.Join(a.Cfg.StackRoot, name)
		item := SyncItem{Stack: name, Kind: SyncOrphanManifest}
		if err := model.ValidateStackName(name); err != nil {
			item.Details, item.Action = []string{err.Error()}, "none (directory name is not a valid stack name)"
			items = append(items, item)
			continue
		}
		if _, _, err := store.ResolveStackDir(a.Cfg.StackRoot, name); err != nil {
			item.Details, item.Action = []string{err.Error()}, "none (manual review required)"
			items = append(items, item)
			continue
		}
		m, err := store.ReadManifest(dir)
		if err != nil {
			item.Details, item.Action = []string{err.Error()}, "none (manifest cannot be imported; fix or remove it manually)"
			items = append(items, item)
			continue
		}
		if m.Stack != name || m.Directory != dir {
			item.Details = []string{fmt.Sprintf("manifest describes stack %q in %s but is stored in %s", m.Stack, m.Directory, dir)}
			item.Action = "none (ambiguous; fix or remove the manifest manually)"
			items = append(items, item)
			continue
		}
		candidate := model.StackFromManifest(m, a.Now())
		proposed.Stacks[name] = candidate
		if err := proposed.Validate(); err != nil {
			delete(proposed.Stacks, name)
			item.Details = []string{fmt.Sprintf("manifest %s is not registered and cannot be imported: %v", store.ManifestPath(dir), err)}
			item.Action = "none (conflicts with the registry; resolve manually)"
			items = append(items, item)
			continue
		}
		item.Details = []string{fmt.Sprintf("manifest %s is valid but stack %q is not registered", store.ManifestPath(dir), name)}
		item.Action = "import the manifest into the registry"
		item.Safe, item.manifest = true, m
		items = append(items, item)
	}
	return items
}

func (a *App) applySync(reg *model.Registry, rep *SyncReport) error {
	now := a.Now()
	proposed := reg.Clone()
	var imports []int
	for i := range rep.Items {
		it := &rep.Items[i]
		if !it.Safe {
			continue
		}
		if it.manifest != nil {
			imports = append(imports, i)
			continue
		}
		path := store.ManifestPath(it.stack.Directory)
		var prev *model.Manifest
		if it.Kind == SyncManifestDiffers {
			prev, _ = store.ReadManifest(it.stack.Directory)
		}
		if it.Kind != SyncManifestMissing {
			dest, err := store.CopyAside(path, "bak", now)
			if err != nil {
				it.Result = fmt.Sprintf("not applied: cannot preserve the existing manifest: %v", err)
				continue
			}
			it.Result = "previous manifest preserved as " + dest + "; "
		}
		if err := store.WriteManifest(it.stack.Directory, model.ManifestFor(it.stack, prev), it.Kind == SyncManifestMissing); err != nil {
			it.Result += fmt.Sprintf("not applied: %v", err)
			continue
		}
		it.Applied = true
		it.Result += "manifest rewritten from the registry"
	}
	if len(imports) == 0 {
		return nil
	}
	for _, i := range imports {
		it := &rep.Items[i]
		proposed.Stacks[it.Stack] = model.StackFromManifest(it.manifest, now)
		if err := proposed.Validate(); err != nil {
			delete(proposed.Stacks, it.Stack)
			it.Result = fmt.Sprintf("not applied: %v", err)
			continue
		}
		it.Applied = true
		it.Result = "imported into the registry"
	}
	proposed.UpdatedAt = now
	if err := a.save(proposed); err != nil {
		for _, i := range imports {
			if rep.Items[i].Applied {
				rep.Items[i].Applied = false
				rep.Items[i].Result = fmt.Sprintf("not applied: registry update failed: %v", err)
			}
		}
		return fmt.Errorf("registry update failed; no manifests were imported: %w", err)
	}
	return nil
}
