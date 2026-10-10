package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cmerk2021/ports/internal/compose"
	"github.com/cmerk2021/ports/internal/config"
	"github.com/cmerk2021/ports/internal/hostports"
	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

type fakeRunner struct {
	out map[string]string // by stack dir
	err error
}

func (f *fakeRunner) Config(_ context.Context, dir string, _ []string) ([]byte, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return []byte(f.out[dir]), nil, nil
}

type env struct {
	*App
	root   string
	runner *fakeRunner
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	cfg := config.Default()
	cfg.StackRoot = filepath.Join(base, "stacks")
	cfg.RegistryPath = filepath.Join(base, "state", "registry.json")
	cfg.ProcNetDir = filepath.Join(base, "no-proc")
	if err := os.Mkdir(cfg.StackRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	a := New(cfg)
	r := &fakeRunner{out: map[string]string{}}
	a.Runner = r
	a.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	a.HostScan = nil
	a.Confirm = func(string) error { return nil }
	if _, err := a.Init(false); err != nil {
		t.Fatal(err)
	}
	return &env{App: a, root: cfg.StackRoot, runner: r}
}

func (e *env) mkdir(t *testing.T, name string) string {
	t.Helper()
	d := filepath.Join(e.root, name)
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) registry(t *testing.T) *model.Registry {
	t.Helper()
	r, err := store.LoadRegistry(e.Cfg.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func blocks(s *model.Stack) []model.Allocation {
	var out []model.Allocation
	for _, a := range s.Allocations {
		if a.Type == model.TypeBlock {
			out = append(out, a)
		}
	}
	return out
}

func mustFixed(t *testing.T, spec string) []model.Allocation {
	t.Helper()
	a, err := model.ParseFixedSpec(spec, "tcp", "0.0.0.0", "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewSequentialAndManifest(t *testing.T) {
	e := newEnv(t)
	for i, name := range []string{"sonarr", "radarr", "lidarr"} {
		e.mkdir(t, name)
		res, err := e.NewStack(name, NewOptions{})
		if err != nil {
			t.Fatal(err)
		}
		b := blocks(res.Stack)
		if len(b) != 1 || b[0].Start != 12000+5*i || b[0].End != 12004+5*i {
			t.Fatalf("%s got %v", name, b)
		}
		m, err := store.ReadManifest(filepath.Join(e.root, name))
		if err != nil {
			t.Fatal(err)
		}
		if d := model.DiffStackManifest(e.registry(t).Stacks[name], m); len(d) > 0 {
			t.Fatalf("manifest differs: %v", d)
		}
	}
}

func TestNewBlockSizeOverrideAndConfig(t *testing.T) {
	e := newEnv(t)
	e.mkdir(t, "a")
	e.mkdir(t, "b")
	res, err := e.NewStack("a", NewOptions{BlockSize: 10})
	if err != nil || blocks(res.Stack)[0].End != 12009 {
		t.Fatalf("%v %v", res, err)
	}
	e.Cfg.BlockSize = 3
	res, err = e.NewStack("b", NewOptions{})
	if err != nil || blocks(res.Stack)[0].Start != 12010 || blocks(res.Stack)[0].End != 12012 {
		t.Fatalf("%v %v", res, err)
	}
}

func TestNewExhaustedRange(t *testing.T) {
	e := newEnv(t)
	e.Cfg.DynamicRange = model.Range{Start: 12000, End: 12009}
	e.mkdir(t, "a")
	e.mkdir(t, "b")
	e.mkdir(t, "c")
	for _, n := range []string{"a", "b"} {
		if _, err := e.NewStack(n, NewOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.NewStack("c", NewOptions{})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(store.ManifestPath(filepath.Join(e.root, "c"))); !os.IsNotExist(err) {
		t.Fatal("manifest created despite failure")
	}
}

func TestNewHybridAndFixedOutsideRange(t *testing.T) {
	e := newEnv(t)
	e.mkdir(t, "nginx-proxy-manager")
	res, err := e.NewStack("nginx-proxy-manager", NewOptions{Fixed: mustFixed(t, "80,81,443")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stack.Allocations) != 4 || res.Stack.Allocations[0].Start != 80 || blocks(res.Stack)[0].Start != 12000 {
		t.Fatalf("%v", res.Stack.Allocations)
	}
	e.mkdir(t, "dns")
	res, err = e.NewStack("dns", NewOptions{Fixed: mustFixed(t, "53/udp,53/tcp"), NoBlock: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks(res.Stack)) != 0 || len(res.Stack.Allocations) != 2 {
		t.Fatalf("%v", res.Stack.Allocations)
	}
	if _, err := e.NewStack("x", NewOptions{NoBlock: true}); !errors.Is(err, ErrUsage) {
		t.Fatalf("--no-block without --fixed: %v", err)
	}
}

func TestNewConflictsAndDuplicates(t *testing.T) {
	e := newEnv(t)
	e.mkdir(t, "jellyfin")
	e.mkdir(t, "other")
	if _, err := e.NewStack("jellyfin", NewOptions{Fixed: mustFixed(t, "8096")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NewStack("jellyfin", NewOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := e.NewStack("other", NewOptions{Fixed: mustFixed(t, "8096")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("fixed overlap: %v", err)
	}
	// Fixed port inside another stack's dynamic block.
	if _, err := e.NewStack("other", NewOptions{Fixed: mustFixed(t, "12002/udp")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("fixed vs dynamic overlap: %v", err)
	}
	// UDP on the same number as a TCP reservation is fine.
	res, err := e.NewStack("other", NewOptions{Fixed: mustFixed(t, "8096/udp")})
	if err != nil {
		t.Fatalf("udp alongside tcp: %v", err)
	}
	// The new block skips the jellyfin block.
	if b := blocks(res.Stack)[0]; b.Start != 12005 {
		t.Fatalf("block %v", b)
	}
	e.mkdir(t, "fixedfirst")
	// A fixed reservation inside the dynamic range is honoured and skipped by
	// the block allocator.
	res, err = e.NewStack("fixedfirst", NewOptions{Fixed: mustFixed(t, "12010")})
	if err != nil || blocks(res.Stack)[0].Start != 12011 {
		t.Fatalf("%v %v", res, err)
	}
}

func TestNewWildcardVsSpecific(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.mkdir(t, n)
	}
	lan, _ := model.ParseFixedSpec("9000", "tcp", "192.168.1.10", "")
	lan2, _ := model.ParseFixedSpec("9000", "tcp", "192.168.1.11", "")
	if _, err := e.NewStack("a", NewOptions{Fixed: lan, NoBlock: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NewStack("b", NewOptions{Fixed: lan2, NoBlock: true}); err != nil {
		t.Fatalf("distinct specific addresses must coexist: %v", err)
	}
	if _, err := e.NewStack("c", NewOptions{Fixed: mustFixed(t, "9000"), NoBlock: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wildcard must conflict with specific: %v", err)
	}
}

func TestNewDirectoryHandling(t *testing.T) {
	e := newEnv(t)
	if _, err := e.NewStack("missing", NewOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing dir: %v", err)
	}
	if _, err := e.NewStack("created", NewOptions{InitDir: true}); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.root, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NewStack("evil", NewOptions{InitDir: true}); !errors.Is(err, store.ErrUnsafePath) {
		t.Fatalf("symlinked stack dir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, ".ports")); !os.IsNotExist(err) {
		t.Fatal("manifest written through symlink")
	}
	if _, err := e.NewStack("../escape", NewOptions{InitDir: true}); !errors.Is(err, ErrUsage) {
		t.Fatalf("traversal: %v", err)
	}
	d := e.mkdir(t, "hasmanifest")
	os.WriteFile(store.ManifestPath(d), []byte("keep me"), 0o644)
	if _, err := e.NewStack("hasmanifest", NewOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing manifest: %v", err)
	}
	if b, _ := os.ReadFile(store.ManifestPath(d)); string(b) != "keep me" {
		t.Fatal("existing manifest overwritten")
	}
}

func TestConcurrentNew(t *testing.T) {
	e := newEnv(t)
	const n = 25
	for i := 0; i < n; i++ {
		e.mkdir(t, fmt.Sprintf("s%02d", i))
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine uses its own App, as separate processes would.
			a := New(e.Cfg)
			a.Runner, a.Now, a.HostScan = e.Runner, e.Now, nil
			_, err := a.NewStack(fmt.Sprintf("s%02d", i), NewOptions{})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	reg := e.registry(t)
	if len(reg.Stacks) != n {
		t.Fatalf("%d stacks registered", len(reg.Stacks))
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("overlapping allocations: %v", err)
	}
	seen := map[int]bool{}
	for _, s := range reg.Stacks {
		b := blocks(s)[0]
		if seen[b.Start] || (b.Start-12000)%5 != 0 || b.Start >= 12000+5*n {
			t.Fatalf("unexpected block %v", b)
		}
		seen[b.Start] = true
	}
}

func TestAssign(t *testing.T) {
	e := newEnv(t)
	e.mkdir(t, "jellyfin")
	if _, err := e.NewStack("jellyfin", NewOptions{Fixed: mustFixed(t, "8096")}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Assign("jellyfin", "web", 8096, AssignOptions{})
	if err != nil || res.Assigned.HostPort != 8096 {
		t.Fatalf("auto assign lowest: %v %v", res, err)
	}
	res, err = e.Assign("jellyfin", "dlna", 1900, AssignOptions{Protocol: "udp"})
	if err != nil || res.Assigned.HostPort != 12000 {
		t.Fatalf("udp auto assign: %v %v", res, err)
	}
	res, err = e.Assign("jellyfin", "https", 8920, AssignOptions{HostPort: 12003})
	if err != nil || res.Assigned.HostPort != 12003 {
		t.Fatalf("explicit host port: %v %v", res, err)
	}
	res, err = e.Assign("jellyfin", "next", 1, AssignOptions{})
	if err != nil || res.Assigned.HostPort != 12000 {
		t.Fatalf("tcp 12000 is free even though udp 12000 is assigned: %v %v", res, err)
	}
	if _, err := e.Assign("jellyfin", "dup", 1, AssignOptions{HostPort: 12003}); !errors.Is(err, ErrConflict) {
		t.Fatalf("already assigned host port: %v", err)
	}
	if _, err := e.Assign("jellyfin", "web", 1, AssignOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := e.Assign("jellyfin", "x", 1, AssignOptions{HostPort: 9999}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "ports reserve") {
		t.Fatalf("outside reservations: %v", err)
	}
	if _, err := e.Assign("jellyfin", "x", 1, AssignOptions{HostPort: 8096, Protocol: "udp"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("tcp-only fixed used for udp: %v", err)
	}
	if _, err := e.Assign("jellyfin", "x", 0, AssignOptions{}); !errors.Is(err, ErrUsage) {
		t.Fatalf("container port 0: %v", err)
	}
	if _, err := e.Assign("nope", "x", 1, AssignOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown stack: %v", err)
	}
	m, _ := store.ReadManifest(filepath.Join(e.root, "jellyfin"))
	if len(m.Assignments) != 4 {
		t.Fatalf("manifest assignments %v", m.Assignments)
	}
	// Exhaust tcp ports: 8096, 12000, 12003 used; 12001, 12002, 12004 remain.
	for i := 0; i < 3; i++ {
		if _, err := e.Assign("jellyfin", fmt.Sprintf("f%d", i), 1, AssignOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Assign("jellyfin", "full", 1, AssignOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("no free port: %v", err)
	}
}

func TestReserveAndRelease(t *testing.T) {
	e := newEnv(t)
	e.mkdir(t, "a")
	e.mkdir(t, "b")
	e.NewStack("a", NewOptions{})
	e.NewStack("b", NewOptions{Fixed: mustFixed(t, "443")})
	if _, err := e.Reserve("a", "443", ReserveOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := e.Reserve("a", "12007", ReserveOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict with other block: %v", err)
	}
	if _, err := e.Reserve("a", "12002", ReserveOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlap with own block: %v", err)
	}
	if _, err := e.Reserve("a", "53", ReserveOptions{Protocol: "udp", Bind: "127.0.0.1", Purpose: "dns"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Assign("a", "dns", 53, AssignOptions{HostPort: 53, Protocol: "udp", Bind: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	// Reservation in use cannot be released.
	if _, err := e.ReleaseReservation("a", 53, false, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("release in-use reservation: %v", err)
	}
	if _, err := e.Release("a", "dns"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Release("a", "dns"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double release: %v", err)
	}
	// The reservation survives the assignment release.
	if s := e.registry(t).Stacks["a"]; len(s.Allocations) != 2 {
		t.Fatalf("allocations %v", s.Allocations)
	}
	// Confirmation is required.
	e.Confirm = func(string) error { return ErrDeclined }
	if _, err := e.ReleaseReservation("a", 53, false, "", ""); !errors.Is(err, ErrDeclined) {
		t.Fatalf("expected declined: %v", err)
	}
	if s := e.registry(t).Stacks["a"]; len(s.Allocations) != 2 {
		t.Fatal("declined release changed state")
	}
	e.Confirm = func(string) error { return nil }
	if _, err := e.ReleaseReservation("a", 53, false, "tcp", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("protocol filter: %v", err)
	}
	if res, err := e.ReleaseReservation("a", 53, false, "udp", ""); err != nil || len(res.Removed) != 1 {
		t.Fatalf("%v %v", res, err)
	}
	if res, err := e.ReleaseReservation("a", 0, true, "", ""); err != nil || len(res.Removed) != 1 {
		t.Fatalf("release block: %v %v", res, err)
	}
	if res, err := e.AddBlock("a", 4); err != nil || res.Added[0].Start != 12000 || res.Added[0].End != 12003 {
		t.Fatalf("add-block reuses freed range: %v %v", res, err)
	}
}

func TestCommitFailureRollsBackManifest(t *testing.T) {
	e := newEnv(t)
	dir := e.mkdir(t, "a")
	e.saveRegistry = func(string, *model.Registry) error { return errors.New("disk full") }
	if _, err := e.NewStack("a", NewOptions{}); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(store.ManifestPath(dir)); !os.IsNotExist(err) {
		t.Fatal("manifest left behind after failed create")
	}
	e.saveRegistry = nil
	if _, err := e.NewStack("a", NewOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.ManifestPath(dir))
	e.saveRegistry = func(string, *model.Registry) error { return errors.New("disk full") }
	if _, err := e.Assign("a", "web", 80, AssignOptions{}); err == nil {
		t.Fatal("expected failure")
	}
	after, _ := os.ReadFile(store.ManifestPath(dir))
	if string(before) != string(after) {
		t.Fatal("manifest not restored after failed registry commit")
	}
	if _, err := e.Delete("a", true); err == nil {
		t.Fatal("expected delete failure")
	}
	if _, err := os.Lstat(store.ManifestPath(dir)); err != nil {
		t.Fatal("manifest not restored after failed delete")
	}
	e.saveRegistry = nil
	if r := e.registry(t); len(r.Stacks["a"].Assignments) != 0 {
		t.Fatal("registry changed")
	}
}

func TestPartialFailureRecoveredBySync(t *testing.T) {
	e := newEnv(t)
	dir := e.mkdir(t, "a")
	if _, err := e.NewStack("a", NewOptions{}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the manifest write but before the registry
	// commit (no rollback happened): the manifest has an extra assignment.
	m, _ := store.ReadManifest(dir)
	m.Assignments = append(m.Assignments, model.Assignment{Name: "web", HostPort: 12000, ContainerPort: 80, Protocol: "tcp", BindAddress: "0.0.0.0", Status: "assigned"})
	if err := store.WriteManifest(dir, m, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Assign("a", "api", 81, AssignOptions{}); !errors.Is(err, ErrInconsistent) {
		t.Fatalf("modifications must be refused while inconsistent: %v", err)
	}
	rep, err := e.Sync(false)
	if err != nil || len(rep.Items) != 1 || rep.Items[0].Kind != SyncManifestDiffers || !rep.Items[0].Safe {
		t.Fatalf("%+v %v", rep, err)
	}
	if after, _ := store.ReadManifest(dir); len(after.Assignments) != 1 {
		t.Fatal("dry run modified the manifest")
	}
	rep, err = e.Sync(true)
	if err != nil || rep.Pending() {
		t.Fatalf("%+v %v", rep, err)
	}
	fixed, _ := store.ReadManifest(dir)
	if len(fixed.Assignments) != 0 {
		t.Fatal("manifest not rewritten from registry")
	}
	entries, _ := os.ReadDir(dir)
	backup := false
	for _, en := range entries {
		backup = backup || strings.HasPrefix(en.Name(), ".ports.bak-")
	}
	if !backup {
		t.Fatal("previous manifest not preserved")
	}
	if _, err := e.Assign("a", "api", 81, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSyncCases(t *testing.T) {
	e := newEnv(t)
	dirA := e.mkdir(t, "a")
	e.mkdir(t, "b")
	dirC := e.mkdir(t, "c")
	e.NewStack("a", NewOptions{})
	e.NewStack("b", NewOptions{})
	os.Remove(store.ManifestPath(dirA))
	os.RemoveAll(filepath.Join(e.root, "b"))
	// An unregistered, valid, non-conflicting manifest (e.g. from a lost registry).
	m := &model.Manifest{Version: 1, Stack: "c", Directory: dirC, Allocations: []model.Allocation{{Type: "fixed", Start: 8096, End: 8096, Protocol: "tcp", BindAddress: "0.0.0.0"}}, Assignments: []model.Assignment{}}
	if err := store.WriteManifest(dirC, m, true); err != nil {
		t.Fatal(err)
	}
	// An unregistered manifest that conflicts with stack a.
	dirD := e.mkdir(t, "d")
	md := &model.Manifest{Version: 1, Stack: "d", Directory: dirD, Allocations: []model.Allocation{{Type: "block", Start: 12000, End: 12004, Protocol: "both", BindAddress: "0.0.0.0"}}, Assignments: []model.Assignment{}}
	store.WriteManifest(dirD, md, true)
	// An unsupported-version manifest.
	dirE := e.mkdir(t, "e")
	os.WriteFile(store.ManifestPath(dirE), []byte(`{"version": 9}`), 0o644)

	rep, err := e.Sync(true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SyncItem{}
	for _, it := range rep.Items {
		got[it.Stack+"/"+it.Kind] = it
	}
	if it := got["a/"+SyncManifestMissing]; !it.Applied {
		t.Fatalf("missing manifest not restored: %+v", rep.Items)
	}
	if it, ok := got["b/"+SyncDirectoryMissing]; !ok || it.Safe || it.Applied {
		t.Fatalf("missing dir: %+v", it)
	}
	if it := got["c/"+SyncOrphanManifest]; !it.Applied {
		t.Fatalf("orphan not imported: %+v", it)
	}
	if it := got["d/"+SyncOrphanManifest]; it.Safe || it.Applied {
		t.Fatalf("conflicting orphan imported: %+v", it)
	}
	if it := got["e/"+SyncOrphanManifest]; it.Safe {
		t.Fatalf("unsupported manifest considered safe: %+v", it)
	}
	if b, _ := os.ReadFile(store.ManifestPath(dirE)); string(b) != `{"version": 9}` {
		t.Fatal("unsupported manifest modified")
	}
	reg := e.registry(t)
	if _, ok := reg.Stacks["c"]; !ok {
		t.Fatal("c not in registry")
	}
	if _, ok := reg.Stacks["d"]; ok {
		t.Fatal("d imported")
	}
	if !rep.Pending() {
		t.Fatal("unresolved items must remain pending")
	}
}

func TestInitRecover(t *testing.T) {
	e := newEnv(t)
	e.mkdir(t, "a")
	e.mkdir(t, "b")
	e.NewStack("a", NewOptions{Fixed: mustFixed(t, "8096")})
	e.NewStack("b", NewOptions{})
	e.Assign("a", "web", 8096, AssignOptions{})
	os.WriteFile(e.Cfg.RegistryPath, []byte("{corrupt"), 0o644)

	if _, err := e.List(); !errors.Is(err, store.ErrCorrupt) {
		t.Fatalf("corrupt registry: %v", err)
	}
	if _, err := e.Init(false); !errors.Is(err, store.ErrCorrupt) {
		t.Fatalf("init without --recover must refuse: %v", err)
	}
	if b, _ := os.ReadFile(e.Cfg.RegistryPath); string(b) != "{corrupt" {
		t.Fatal("corrupt registry overwritten without --recover")
	}
	res, err := e.Init(true)
	if err != nil || !res.Recovered || len(res.Imported) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(res.Preserved); string(b) != "{corrupt" {
		t.Fatal("corrupt registry not preserved")
	}
	reg := e.registry(t)
	if len(reg.Stacks["a"].Assignments) != 1 || len(reg.Stacks["b"].Allocations) != 1 {
		t.Fatalf("state lost: %+v", reg.Stacks)
	}
	// Missing registry + recover also rebuilds.
	os.Remove(e.Cfg.RegistryPath)
	if res, err := e.Init(true); err != nil || len(res.Imported) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	// Unsupported version is never touched.
	os.WriteFile(e.Cfg.RegistryPath, []byte(`{"version":5}`), 0o644)
	if _, err := e.Init(true); !errors.Is(err, store.ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	dir := e.mkdir(t, "a")
	os.WriteFile(filepath.Join(dir, "data.db"), []byte("data"), 0o644)
	e.NewStack("a", NewOptions{Fixed: mustFixed(t, "80")})
	if _, err := e.Delete("a", false); !errors.Is(err, ErrDeclined) || !strings.Contains(err.Error(), "--release-ports") {
		t.Fatalf("got %v", err)
	}
	e.Confirm = func(string) error { return ErrDeclined }
	if _, err := e.Delete("a", true); !errors.Is(err, ErrDeclined) {
		t.Fatalf("got %v", err)
	}
	if _, ok := e.registry(t).Stacks["a"]; !ok {
		t.Fatal("declined delete removed the stack")
	}
	e.Confirm = func(string) error { return nil }
	res, err := e.Delete("a", true)
	if err != nil || res.ManifestArchived == "" {
		t.Fatalf("%v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "data.db")); string(b) != "data" {
		t.Fatal("application data touched")
	}
	if _, ok := e.registry(t).Stacks["a"]; ok {
		t.Fatal("stack still registered")
	}
	// Ports are free again, and the stack can be re-registered.
	if _, err := e.NewStack("a", NewOptions{Fixed: mustFixed(t, "80")}); err != nil {
		t.Fatal(err)
	}
}

func TestLookupFindAndDockerUnavailable(t *testing.T) {
	e := newEnv(t)
	dir := e.mkdir(t, "jellyfin")
	e.NewStack("jellyfin", NewOptions{Fixed: mustFixed(t, "8096")})
	e.Assign("jellyfin", "web", 8096, AssignOptions{})
	e.runner.err = fmt.Errorf("%w: docker not found", compose.ErrUnavailable)
	info, err := e.Lookup(context.Background(), "jellyfin", true)
	if err != nil {
		t.Fatalf("lookup must work without docker: %v", err)
	}
	if info.ComposeStatus != ComposeStatusNoTool || info.ManifestStatus != ManifestOK || len(info.Stack.Assignments) != 1 {
		t.Fatalf("%+v", info)
	}
	// Discrepancy is reported by lookup.
	m, _ := store.ReadManifest(dir)
	m.Assignments = nil
	store.WriteManifest(dir, m, false)
	info, _ = e.Lookup(context.Background(), "jellyfin", false)
	if info.ManifestStatus != ManifestDiffers || len(info.Discrepancies) == 0 {
		t.Fatalf("%+v", info)
	}
	matches, err := e.Find(8096, "", "")
	if err != nil || len(matches) != 1 || matches[0].Stack != "jellyfin" || len(matches[0].Assignments) != 1 {
		t.Fatalf("%+v %v", matches, err)
	}
	if _, err := e.Find(8096, "udp", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("protocol filter: %v", err)
	}
	if m, err := e.Find(12002, "udp", ""); err != nil || len(m) != 1 {
		t.Fatalf("block covers udp: %v %v", m, err)
	}
	if _, err := e.Find(70000, "", ""); !errors.Is(err, ErrUsage) {
		t.Fatalf("invalid port: %v", err)
	}
}

func TestAuditAndRecord(t *testing.T) {
	e := newEnv(t)
	dir := e.mkdir(t, "jellyfin")
	e.NewStack("jellyfin", NewOptions{Fixed: mustFixed(t, "8096")})
	e.Assign("jellyfin", "web", 8096, AssignOptions{})
	e.runner.out[dir] = `{"services":{"jf":{"ports":[{"target":8096,"published":"8096","protocol":"tcp"}]}}}`
	rep, err := e.Audit(context.Background(), AuditOptions{Record: true})
	if err != nil || rep.Summary.Violations+rep.Summary.Errors != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	m, _ := store.ReadManifest(dir)
	if m.Observed == nil || len(m.Observed.Mappings) != 1 {
		t.Fatalf("observation not recorded: %+v", m.Observed)
	}
	// Recording observations does not create a discrepancy.
	if _, err := e.Assign("jellyfin", "x", 1, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	m, _ = store.ReadManifest(dir)
	if m.Observed == nil {
		t.Fatal("observation lost on update")
	}
	e.runner.out[dir] = `{"services":{"jf":{"ports":[{"target":8096,"published":"7000"}]}}}`
	rep, _ = e.Audit(context.Background(), AuditOptions{Stacks: []string{"jellyfin"}})
	if rep.Summary.Violations == 0 {
		t.Fatalf("%+v", rep)
	}
	// Host listeners are considered when allocating new blocks.
	e.HostScan = func() *hostports.Snapshot {
		return &hostports.Snapshot{Listeners: []hostports.Listener{{Protocol: "tcp", BindAddress: "127.0.0.1", Port: 12006}}}
	}
	e.mkdir(t, "next")
	res, err := e.NewStack("next", NewOptions{})
	if err != nil || blocks(res.Stack)[0].Start != 12007 {
		t.Fatalf("block should skip host listener at 12006: %v %v", res, err)
	}
}
