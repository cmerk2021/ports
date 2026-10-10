// Package app implements the ports operations on top of the model, store,
// compose, hostports and audit packages. All mutations follow the same
// protocol:
//
//  1. take the exclusive registry lock;
//  2. load and validate the registry (and the stack's manifest);
//  3. build and validate the complete proposed registry;
//  4. write the stack manifest atomically (temp file + rename);
//  5. commit the registry atomically (temp file + rename) - this is the
//     commit point;
//  6. if step 5 fails, restore the previous manifest content.
//
// Two files cannot be updated atomically together. If both the registry write
// and the manifest rollback fail, the registry (authoritative) still holds the
// previous state and `ports sync` reports and repairs the manifest.
package app

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/cmerk2021/ports/internal/compose"
	"github.com/cmerk2021/ports/internal/config"
	"github.com/cmerk2021/ports/internal/hostports"
	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

// Error kinds, mapped to exit codes by the CLI.
var (
	ErrUsage        = errors.New("usage error")
	ErrConflict     = errors.New("conflict")
	ErrNotFound     = errors.New("not found")
	ErrInconsistent = errors.New("registry and manifest are inconsistent")
	ErrDeclined     = errors.New("confirmation required")
)

type kindErr struct {
	kind error
	err  error
}

func (e *kindErr) Error() string        { return e.err.Error() }
func (e *kindErr) Unwrap() error        { return e.err }
func (e *kindErr) Is(target error) bool { return target == e.kind }

func errKind(kind error, format string, args ...any) error {
	return &kindErr{kind: kind, err: fmt.Errorf(format, args...)}
}

// App bundles configuration and injectable dependencies.
type App struct {
	Cfg    *config.Config
	Runner compose.Runner
	Now    func() time.Time
	// Confirm is called (while the registry lock is held) before destructive
	// operations. It must return nil to proceed.
	Confirm func(prompt string) error
	// HostScan inspects host listeners; nil disables host inspection.
	HostScan func() *hostports.Snapshot

	// saveRegistry is replaceable by tests to simulate commit failures.
	saveRegistry func(path string, reg *model.Registry) error
}

// New returns an App using real Docker and /proc inspection.
func New(cfg *config.Config) *App {
	return &App{
		Cfg:    cfg,
		Runner: compose.ExecRunner{Command: cfg.DockerCommand},
		Now:    func() time.Time { return time.Now().UTC().Truncate(time.Second) },
		Confirm: func(string) error {
			return errKind(ErrDeclined, "this operation requires confirmation; re-run with --yes")
		},
		HostScan: func() *hostports.Snapshot { return hostports.Scan(cfg.ProcNetDir) },
	}
}

func (a *App) save(reg *model.Registry) error {
	if a.saveRegistry != nil {
		return a.saveRegistry(a.Cfg.RegistryPath, reg)
	}
	return store.SaveRegistry(a.Cfg.RegistryPath, reg)
}

func (a *App) host() *hostports.Snapshot {
	if a.HostScan == nil {
		return nil
	}
	return a.HostScan()
}

// readRegistry loads the registry under a shared lock.
func (a *App) readRegistry() (*model.Registry, error) {
	lock, err := store.AcquireLock(a.Cfg.RegistryPath, false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return store.LoadRegistry(a.Cfg.RegistryPath)
}

// withRegistry runs fn with the registry loaded under the exclusive lock.
func (a *App) withRegistry(fn func(reg *model.Registry) error) error {
	lock, err := store.AcquireLock(a.Cfg.RegistryPath, true)
	if err != nil {
		return err
	}
	defer lock.Release()
	reg, err := store.LoadRegistry(a.Cfg.RegistryPath)
	if err != nil {
		return err
	}
	return fn(reg)
}

func lookupStack(reg *model.Registry, name string) (*model.Stack, error) {
	if err := model.ValidateStackName(name); err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	s, ok := reg.Stacks[name]
	if !ok {
		if _, err := strconv.Atoi(name); err == nil {
			return nil, errKind(ErrNotFound, "stack %q is not registered (to look up a port use `ports find %s`)", name, name)
		}
		return nil, errKind(ErrNotFound, "stack %q is not registered (see `ports list`)", name)
	}
	return s, nil
}

// requireConsistent refuses to modify a stack whose manifest is missing,
// unreadable or disagrees with the registry.
func requireConsistent(s *model.Stack) error {
	m, err := store.ReadManifest(s.Directory)
	if err != nil {
		return errKind(ErrInconsistent, "cannot use manifest for stack %q: %v; run `ports sync --dry-run` to review and `ports sync --apply` to repair", s.Name, err)
	}
	if diffs := model.DiffStackManifest(s, m); len(diffs) > 0 {
		return errKind(ErrInconsistent, "manifest %s disagrees with the registry (%s); run `ports sync --dry-run` to review and `ports sync --apply` to repair", store.ManifestPath(s.Directory), diffs[0])
	}
	return nil
}

// commitUpdate writes the updated stack's manifest and then the proposed
// registry, restoring the previous manifest if the registry commit fails.
func (a *App) commitUpdate(proposed *model.Registry, s *model.Stack) error {
	now := a.Now()
	s.UpdatedAt = now
	proposed.UpdatedAt = now
	s.Normalize()
	if err := proposed.Validate(); err != nil {
		return errKind(ErrConflict, "proposed change is invalid: %v", err)
	}
	prevBytes, err := store.ReadManifestBytes(s.Directory)
	if err != nil {
		return fmt.Errorf("cannot read current manifest: %w", err)
	}
	var prev *model.Manifest
	if prevBytes != nil {
		prev, _ = store.ReadManifest(s.Directory)
	}
	if err := store.WriteManifest(s.Directory, model.ManifestFor(s, prev), false); err != nil {
		return fmt.Errorf("cannot write manifest (nothing was changed): %w", err)
	}
	if err := a.save(proposed); err != nil {
		if prevBytes == nil {
			if rerr := store.RemoveManifest(s.Directory); rerr != nil {
				return fmt.Errorf("registry update failed: %v; additionally the manifest rollback failed: %v (the registry is unchanged; run `ports sync --dry-run`)", err, rerr)
			}
		} else if rerr := store.RestoreManifestBytes(s.Directory, prevBytes); rerr != nil {
			return fmt.Errorf("registry update failed: %v; additionally the manifest rollback failed: %v (the registry is unchanged and authoritative; run `ports sync --apply` to rewrite the manifest)", err, rerr)
		}
		return fmt.Errorf("registry update failed; the manifest was rolled back and nothing was changed: %w", err)
	}
	return nil
}

// commitCreate creates the manifest for a new stack (never overwriting) and
// then commits the registry, removing the new manifest on failure.
func (a *App) commitCreate(proposed *model.Registry, s *model.Stack) error {
	s.Normalize()
	if err := proposed.Validate(); err != nil {
		return errKind(ErrConflict, "proposed change is invalid: %v", err)
	}
	if err := store.WriteManifest(s.Directory, model.ManifestFor(s, nil), true); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errKind(ErrConflict, "a manifest already exists at %s; refusing to overwrite it (use `ports sync` to import or review it)", store.ManifestPath(s.Directory))
		}
		return fmt.Errorf("cannot create manifest (nothing was changed): %w", err)
	}
	if err := a.save(proposed); err != nil {
		if rerr := store.RemoveManifest(s.Directory); rerr != nil {
			return fmt.Errorf("registry update failed: %v; additionally the new manifest could not be removed: %v (run `ports sync --dry-run` to review)", err, rerr)
		}
		return fmt.Errorf("registry update failed; the new manifest was removed and nothing was changed: %w", err)
	}
	return nil
}
