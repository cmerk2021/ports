package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cmerk2021/ports/internal/hostports"
	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

// InitResult describes what `ports init` did.
type InitResult struct {
	RegistryPath string   `json:"registry_path"`
	Created      bool     `json:"created"`
	Recovered    bool     `json:"recovered"`
	Preserved    string   `json:"preserved_as,omitempty"`
	Imported     []string `json:"imported,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// Init creates an empty registry if none exists. With recover set, a corrupt
// registry is preserved under a new name and rebuilt from the stack
// manifests, provided they are mutually consistent.
func (a *App) Init(recover bool) (*InitResult, error) {
	path := a.Cfg.RegistryPath
	res := &InitResult{RegistryPath: path}
	if fi, err := os.Stat(a.Cfg.StackRoot); err != nil || !fi.IsDir() {
		res.Warnings = append(res.Warnings, fmt.Sprintf("stack root %s does not exist or is not a directory; create it before registering stacks", a.Cfg.StackRoot))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create registry directory: %w", err)
	}
	lock, err := store.AcquireLock(path, true)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	_, loadErr := store.LoadRegistry(path)
	switch {
	case loadErr == nil:
		return res, nil
	case errors.Is(loadErr, store.ErrRegistryNotFound):
		if recover {
			return a.rebuildRegistry(res, "")
		}
		if err := store.CreateRegistry(path, a.Now()); err != nil {
			return nil, err
		}
		res.Created = true
		return res, nil
	case errors.Is(loadErr, store.ErrUnsupported):
		return nil, loadErr
	default:
		if !recover {
			return nil, loadErr
		}
		return a.rebuildRegistry(res, path)
	}
}

func (a *App) rebuildRegistry(res *InitResult, corruptPath string) (*InitResult, error) {
	now := a.Now()
	reg := model.NewRegistry(now)
	names, err := store.ScanManifests(a.Cfg.StackRoot)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, name := range names {
		dir, _, err := store.ResolveStackDir(a.Cfg.StackRoot, name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		m, err := store.ReadManifest(dir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if m.Stack != name || m.Directory != dir {
			problems = append(problems, fmt.Sprintf("%s: manifest names stack %q in %s, expected %q in %s", name, m.Stack, m.Directory, name, dir))
			continue
		}
		reg.Stacks[name] = model.StackFromManifest(m, now)
		res.Imported = append(res.Imported, name)
	}
	if len(problems) == 0 {
		if err := reg.Validate(); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return nil, errKind(ErrConflict, "cannot rebuild the registry from manifests; nothing was changed:\n  %s", strings.Join(problems, "\n  "))
	}
	if corruptPath != "" {
		dest, err := store.CopyAside(corruptPath, "corrupt", now)
		if err != nil {
			return nil, err
		}
		res.Preserved = dest
	}
	if err := a.save(reg); err != nil {
		return nil, err
	}
	res.Created, res.Recovered = true, true
	return res, nil
}

// NewOptions configures stack creation.
type NewOptions struct {
	Fixed        []model.Allocation
	BlockSize    int // 0 means the configured default
	NoBlock      bool
	InitDir      bool
	ComposeFiles []string
}

// Result is a generic mutation result.
type Result struct {
	Stack    *model.Stack       `json:"stack"`
	Added    []model.Allocation `json:"added_allocations,omitempty"`
	Removed  []model.Allocation `json:"removed_allocations,omitempty"`
	Assigned *model.Assignment  `json:"assignment,omitempty"`
	Warnings []string           `json:"warnings,omitempty"`
}

// NewStack registers a stack with fixed ports and/or a dynamic block.
func (a *App) NewStack(name string, opt NewOptions) (*Result, error) {
	if err := model.ValidateStackName(name); err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	if opt.NoBlock && len(opt.Fixed) == 0 {
		return nil, errKind(ErrUsage, "--no-block requires at least one --fixed port")
	}
	if opt.NoBlock && opt.BlockSize != 0 {
		return nil, errKind(ErrUsage, "--no-block and --block-size cannot be combined")
	}
	size := opt.BlockSize
	if size == 0 {
		size = a.Cfg.BlockSize
	}
	if size < 1 {
		return nil, errKind(ErrUsage, "invalid block size %d: must be at least 1", size)
	}
	for _, f := range opt.ComposeFiles {
		if strings.TrimSpace(f) == "" {
			return nil, errKind(ErrUsage, "empty --compose-file value")
		}
	}

	var res *Result
	err := a.withRegistry(func(reg *model.Registry) (err error) {
		if _, exists := reg.Stacks[name]; exists {
			return errKind(ErrConflict, "stack %q is already registered (see `ports %s`)", name, name)
		}
		dir, exists, err := store.ResolveStackDir(a.Cfg.StackRoot, name)
		if err != nil {
			return err
		}
		if !exists {
			if !opt.InitDir {
				return errKind(ErrNotFound, "stack directory %s does not exist; create it first or pass --init-dir", dir)
			}
			if dir, err = store.CreateStackDir(a.Cfg.StackRoot, name); err != nil {
				return err
			}
			defer func() {
				if err != nil {
					// Still under the lock; only succeeds if the directory is empty.
					_ = os.Remove(dir)
				}
			}()
		}
		if ok, err := store.ManifestExists(dir); err != nil {
			return err
		} else if ok {
			return errKind(ErrConflict, "%s already exists but stack %q is not in the registry; refusing to overwrite it (run `ports sync --dry-run` to review or import it)", store.ManifestPath(dir), name)
		}
		for _, other := range reg.Stacks {
			if other.Directory == dir {
				return errKind(ErrConflict, "directory %s is already registered to stack %q", dir, other.Name)
			}
		}

		now := a.Now()
		s := &model.Stack{Name: name, Directory: dir, ComposeFiles: opt.ComposeFiles, CreatedAt: now, UpdatedAt: now}
		res = &Result{Stack: s}
		host := a.host()
		for i, f := range opt.Fixed {
			if err := f.Validate(); err != nil {
				return errKind(ErrUsage, "%v", err)
			}
			for _, g := range opt.Fixed[:i] {
				if f.Conflicts(g) {
					return errKind(ErrUsage, "fixed reservations %s and %s overlap", g, f)
				}
			}
			if cs := reg.ConflictsWith(f, name); len(cs) > 0 {
				return conflictError(f, cs)
			}
			res.Warnings = append(res.Warnings, hostWarnings(host, f)...)
			s.Allocations = append(s.Allocations, f)
		}
		if !opt.NoBlock {
			b, err := a.allocateBlock(reg, s.Allocations, size, host)
			if err != nil {
				return err
			}
			s.Allocations = append(s.Allocations, b)
		}
		res.Added = append([]model.Allocation{}, s.Allocations...)
		proposed := reg.Clone()
		proposed.Stacks[name] = s
		proposed.UpdatedAt = now
		return a.commitCreate(proposed, s)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// allocateBlock finds a free first-fit block. Ports with current host
// listeners inside the dynamic range are skipped as well: absence from the
// registry does not mean a port is free.
func (a *App) allocateBlock(reg *model.Registry, extra []model.Allocation, size int, host *hostports.Snapshot) (model.Allocation, error) {
	busy := append(reg.AllAllocations(), extra...)
	if host != nil {
		for _, l := range host.Listeners {
			busy = append(busy, model.Allocation{Start: l.Port, End: l.Port})
		}
	}
	start, err := model.FindBlock(busy, a.Cfg.DynamicRange, size)
	if err != nil {
		if errors.Is(err, model.ErrRangeExhausted) {
			return model.Allocation{}, errKind(ErrConflict, "%v; widen dynamic_range in the configuration, use a smaller --block-size, or release unused stacks", err)
		}
		return model.Allocation{}, errKind(ErrUsage, "%v", err)
	}
	return model.Allocation{Type: model.TypeBlock, Start: start, End: start + size - 1, Protocol: model.ProtoBoth, BindAddress: model.WildcardIPv4, Purpose: "expansion"}, nil
}

func conflictError(a model.Allocation, cs []model.Conflict) error {
	var parts []string
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s [%s]", c.StackB, c.B))
	}
	return errKind(ErrConflict, "%s conflicts with existing reservation(s): %s; fixed ports are never moved automatically", a, strings.Join(parts, ", "))
}

func hostWarnings(host *hostports.Snapshot, a model.Allocation) []string {
	if host == nil {
		return nil
	}
	var out []string
	for p := a.Start; p <= a.End; p++ {
		for _, proto := range []string{model.ProtoTCP, model.ProtoUDP} {
			if !model.ProtocolCovers(a.Protocol, proto) {
				continue
			}
			for _, l := range host.Matching(p, proto, a.BindAddress) {
				out = append(out, fmt.Sprintf("%s:%d/%s is currently in use on the host by an unknown process", l.BindAddress, l.Port, l.Protocol))
			}
		}
	}
	return out
}

// AddBlock allocates an additional dynamic block for an existing stack.
func (a *App) AddBlock(name string, size int) (*Result, error) {
	if size == 0 {
		size = a.Cfg.BlockSize
	}
	if size < 1 {
		return nil, errKind(ErrUsage, "invalid block size %d: must be at least 1", size)
	}
	var res *Result
	err := a.withRegistry(func(reg *model.Registry) error {
		cur, err := lookupStack(reg, name)
		if err != nil {
			return err
		}
		if err := requireConsistent(cur); err != nil {
			return err
		}
		b, err := a.allocateBlock(reg, nil, size, a.host())
		if err != nil {
			return err
		}
		proposed := reg.Clone()
		s := proposed.Stacks[name]
		s.Allocations = append(s.Allocations, b)
		res = &Result{Stack: s, Added: []model.Allocation{b}}
		return a.commitUpdate(proposed, s)
	})
	return res, err
}

// ReserveOptions configures a fixed reservation.
type ReserveOptions struct {
	Protocol string
	Bind     string
	Purpose  string
}

// Reserve adds fixed reservations (spec like "80", "8000-8005" or "53/udp").
func (a *App) Reserve(name, spec string, opt ReserveOptions) (*Result, error) {
	proto, err := model.NormalizeProtocol(opt.Protocol, model.ProtoTCP, true)
	if err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	bind, err := model.NormalizeBind(opt.Bind)
	if err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	allocs, err := model.ParseFixedSpec(spec, proto, bind, opt.Purpose)
	if err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	var res *Result
	err = a.withRegistry(func(reg *model.Registry) error {
		cur, err := lookupStack(reg, name)
		if err != nil {
			return err
		}
		if err := requireConsistent(cur); err != nil {
			return err
		}
		proposed := reg.Clone()
		s := proposed.Stacks[name]
		res = &Result{Stack: s}
		host := a.host()
		for _, f := range allocs {
			if err := f.Validate(); err != nil {
				return errKind(ErrUsage, "%v", err)
			}
			for _, own := range s.Allocations {
				if own.Conflicts(f) {
					return errKind(ErrConflict, "%s overlaps the stack's existing reservation %s", f, own)
				}
			}
			if cs := proposed.ConflictsWith(f, name); len(cs) > 0 {
				return conflictError(f, cs)
			}
			res.Warnings = append(res.Warnings, hostWarnings(host, f)...)
			s.Allocations = append(s.Allocations, f)
			res.Added = append(res.Added, f)
		}
		return a.commitUpdate(proposed, s)
	})
	return res, err
}

// AssignOptions configures an assignment.
type AssignOptions struct {
	HostPort int // 0 selects the lowest free reserved port
	Protocol string
	Bind     string
	Purpose  string
}

// Assign records an intended host-port -> container-port mapping.
func (a *App) Assign(name, assignment string, containerPort int, opt AssignOptions) (*Result, error) {
	if err := model.ValidateAssignmentName(assignment); err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	if err := model.ValidatePort(containerPort); err != nil {
		return nil, errKind(ErrUsage, "container port: %v", err)
	}
	if opt.HostPort != 0 {
		if err := model.ValidatePort(opt.HostPort); err != nil {
			return nil, errKind(ErrUsage, "host port: %v", err)
		}
	}
	proto, err := model.NormalizeProtocol(opt.Protocol, model.ProtoTCP, false)
	if err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	bind, err := model.NormalizeBind(opt.Bind)
	if err != nil {
		return nil, errKind(ErrUsage, "%v", err)
	}
	var res *Result
	err = a.withRegistry(func(reg *model.Registry) error {
		cur, err := lookupStack(reg, name)
		if err != nil {
			return err
		}
		if err := requireConsistent(cur); err != nil {
			return err
		}
		if cur.FindAssignment(assignment) >= 0 {
			return errKind(ErrConflict, "stack %q already has an assignment named %q (release it first)", name, assignment)
		}
		used := func(p int) *model.Assignment {
			for i, as := range cur.Assignments {
				if as.HostPort == p && as.Protocol == proto && model.BindsOverlap(as.BindAddress, bind) {
					return &cur.Assignments[i]
				}
			}
			return nil
		}
		host := a.host()
		port := opt.HostPort
		if port != 0 {
			if _, ok := cur.Covering(port, proto, bind); !ok {
				return errKind(ErrConflict, "host port %s:%d/%s is not reserved by stack %q; reserve it first with `ports reserve %s %d --protocol %s` (or choose a port from `ports %s`)", bind, port, proto, name, name, port, proto, name)
			}
			if u := used(port); u != nil {
				return errKind(ErrConflict, "host port %d/%s is already assigned to %q in stack %q", port, proto, u.Name, name)
			}
		} else {
			port = lowestFree(cur, proto, bind, used)
			if port == 0 {
				return errKind(ErrConflict, "stack %q has no unassigned reserved %s port for bind address %s; reserve more with `ports reserve` or `ports add-block`", name, proto, bind)
			}
		}
		as := model.Assignment{Name: assignment, HostPort: port, ContainerPort: containerPort, Protocol: proto, BindAddress: bind, Purpose: opt.Purpose, Status: model.AssignmentStatusAssigned}
		proposed := reg.Clone()
		s := proposed.Stacks[name]
		s.Assignments = append(s.Assignments, as)
		res = &Result{Stack: s, Assigned: &as}
		if host != nil {
			for _, l := range host.Matching(port, proto, bind) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s:%d/%s is currently in use on the host by an unknown process (possibly this stack)", l.BindAddress, l.Port, l.Protocol))
			}
		}
		return a.commitUpdate(proposed, s)
	})
	return res, err
}

func lowestFree(s *model.Stack, proto, bind string, used func(int) *model.Assignment) int {
	best := 0
	for _, al := range s.Allocations {
		if !model.ProtocolCovers(al.Protocol, proto) || !model.BindCovers(al.BindAddress, bind) {
			continue
		}
		for p := al.Start; p <= al.End; p++ {
			if best != 0 && p >= best {
				break
			}
			if used(p) == nil {
				best = p
				break
			}
		}
	}
	return best
}

// Release removes an assignment. The underlying reservation is kept.
func (a *App) Release(name, assignment string) (*Result, error) {
	var res *Result
	err := a.withRegistry(func(reg *model.Registry) error {
		cur, err := lookupStack(reg, name)
		if err != nil {
			return err
		}
		if err := requireConsistent(cur); err != nil {
			return err
		}
		i := cur.FindAssignment(assignment)
		if i < 0 {
			return errKind(ErrNotFound, "stack %q has no assignment named %q", name, assignment)
		}
		proposed := reg.Clone()
		s := proposed.Stacks[name]
		removed := s.Assignments[i]
		s.Assignments = append(s.Assignments[:i], s.Assignments[i+1:]...)
		res = &Result{Stack: s, Assigned: &removed}
		return a.commitUpdate(proposed, s)
	})
	return res, err
}

// ReleaseReservation removes fixed reservations containing port (filtered by
// protocol/bind when given) or, with block set, the stack's dynamic blocks.
// Reservations still used by assignments are never removed.
func (a *App) ReleaseReservation(name string, port int, block bool, protocol, bind string) (*Result, error) {
	if !block {
		if err := model.ValidatePort(port); err != nil {
			return nil, errKind(ErrUsage, "%v", err)
		}
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
	var res *Result
	err := a.withRegistry(func(reg *model.Registry) error {
		cur, err := lookupStack(reg, name)
		if err != nil {
			return err
		}
		if err := requireConsistent(cur); err != nil {
			return err
		}
		proposed := reg.Clone()
		s := proposed.Stacks[name]
		var keep, removed []model.Allocation
		for _, al := range s.Allocations {
			match := false
			if block {
				match = al.Type == model.TypeBlock
			} else {
				match = al.Type == model.TypeFixed && al.Contains(port) &&
					(protocol == "" || al.Protocol == protocol) && (bind == "" || al.BindAddress == bind)
			}
			if match {
				removed = append(removed, al)
			} else {
				keep = append(keep, al)
			}
		}
		if len(removed) == 0 {
			if block {
				return errKind(ErrNotFound, "stack %q has no dynamic block", name)
			}
			return errKind(ErrNotFound, "stack %q has no fixed reservation containing port %d matching the given protocol/bind address", name, port)
		}
		s.Allocations = keep
		for _, as := range s.Assignments {
			if _, ok := s.Covering(as.HostPort, as.Protocol, as.BindAddress); !ok {
				return errKind(ErrConflict, "assignment %q (%d/%s) uses the reservation; release the assignment first with `ports release %s %s`", as.Name, as.HostPort, as.Protocol, name, as.Name)
			}
		}
		var desc []string
		for _, r := range removed {
			desc = append(desc, r.String())
		}
		if err := a.Confirm(fmt.Sprintf("Release %s from stack %q? Released ports may be allocated to other stacks.", strings.Join(desc, ", "), name)); err != nil {
			return err
		}
		res = &Result{Stack: s, Removed: removed}
		return a.commitUpdate(proposed, s)
	})
	return res, err
}

// DeleteResult describes a deleted registration.
type DeleteResult struct {
	Stack            *model.Stack `json:"stack"`
	ManifestArchived string       `json:"manifest_archived_as,omitempty"`
}

// Delete removes a stack registration and frees all of its reservations. The
// stack directory and application data are never touched; the manifest is
// renamed to ".ports.deleted-<timestamp>" for reference. releasePorts must be
// set to acknowledge that the reservations are freed.
func (a *App) Delete(name string, releasePorts bool) (*DeleteResult, error) {
	var res *DeleteResult
	err := a.withRegistry(func(reg *model.Registry) error {
		cur, err := lookupStack(reg, name)
		if err != nil {
			return err
		}
		var desc []string
		for _, al := range cur.Allocations {
			desc = append(desc, al.String())
		}
		summary := strings.Join(desc, "; ")
		if summary == "" {
			summary = "no reservations"
		}
		if !releasePorts {
			return errKind(ErrDeclined, "deleting stack %q would free its reservations (%s) for other stacks; re-run with --release-ports to confirm (application data in %s is never deleted)", name, summary, cur.Directory)
		}
		if err := a.Confirm(fmt.Sprintf("Delete registration of stack %q and free %s? Data in %s is kept.", name, summary, cur.Directory)); err != nil {
			return err
		}
		proposed := reg.Clone()
		delete(proposed.Stacks, name)
		proposed.UpdatedAt = a.Now()
		if err := proposed.Validate(); err != nil {
			return err
		}
		res = &DeleteResult{Stack: cur}
		mpath := store.ManifestPath(cur.Directory)
		archived := ""
		if fi, err := os.Lstat(mpath); err == nil && fi.Mode().IsRegular() {
			if archived, err = store.MoveAside(mpath, "deleted", a.Now()); err != nil {
				return fmt.Errorf("cannot archive manifest (nothing was changed): %w", err)
			}
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cannot inspect manifest %s: %w", mpath, err)
		}
		if err := a.save(proposed); err != nil {
			if archived != "" {
				if rerr := os.Rename(archived, mpath); rerr != nil {
					return fmt.Errorf("registry update failed: %v; the manifest remains archived at %s (the registry is unchanged; run `ports sync --apply` to restore it)", err, archived)
				}
			}
			return fmt.Errorf("registry update failed; nothing was changed: %w", err)
		}
		res.ManifestArchived = archived
		return nil
	})
	return res, err
}
