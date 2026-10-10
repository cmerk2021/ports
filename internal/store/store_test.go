package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cmerk2021/ports/internal/model"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestAtomicWriteReplaceAndNoReplace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	if err := WriteFileAtomic(p, []byte("one"), 0o644, true); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("two"), 0o644, true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("noReplace must refuse to overwrite, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "one" {
		t.Fatalf("content changed: %q", b)
	}
	if err := WriteFileAtomic(p, []byte("three"), 0o644, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "three" {
		t.Fatalf("content = %q", b)
	}
	assertNoTemps(t, dir)
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestAtomicWriteFailureLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	testHookBeforeCommit = func(string) error { return errors.New("simulated crash before rename") }
	defer func() { testHookBeforeCommit = nil }()
	if err := WriteFileAtomic(p, []byte("new"), 0o644, false); err == nil {
		t.Fatal("expected failure")
	}
	if b, _ := os.ReadFile(p); string(b) != "original" {
		t.Fatalf("original modified: %q", b)
	}
	assertNoTemps(t, dir)
}

func TestAtomicWriteUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if err := WriteFileAtomic(filepath.Join(dir, "x"), []byte("x"), 0o644, false); err == nil {
		t.Fatal("expected failure in read-only directory")
	}
}

func TestRegistryLifecycle(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state", "registry.json")
	if _, err := LoadRegistry(p); !errors.Is(err, ErrRegistryNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := CreateRegistry(p, t0); err != nil {
		t.Fatal(err)
	}
	if err := CreateRegistry(p, t0); err == nil {
		t.Fatal("CreateRegistry must not overwrite")
	}
	reg, err := LoadRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	reg.Stacks["a"] = &model.Stack{Name: "a", Directory: "/x/a", Allocations: []model.Allocation{{Type: "block", Start: 12000, End: 12004, Protocol: "both", BindAddress: "0.0.0.0"}}}
	if err := SaveRegistry(p, reg); err != nil {
		t.Fatal(err)
	}
	reg2, err := LoadRegistry(p)
	if err != nil || len(reg2.Stacks) != 1 {
		t.Fatalf("reload: %v %v", reg2, err)
	}
	// Invalid proposed registries are never written.
	reg2.Stacks["b"] = &model.Stack{Name: "b", Directory: "/x/b", Allocations: []model.Allocation{{Type: "block", Start: 12004, End: 12008, Protocol: "both", BindAddress: "0.0.0.0"}}}
	if err := SaveRegistry(p, reg2); err == nil {
		t.Fatal("overlapping registry was saved")
	}
	if reg3, _ := LoadRegistry(p); len(reg3.Stacks) != 1 {
		t.Fatal("invalid registry reached disk")
	}
}

func TestLoadRegistryRejectsBadFiles(t *testing.T) {
	cases := map[string]struct {
		content string
		want    error
	}{
		"garbage":        {"not json", ErrCorrupt},
		"truncated":      {`{"version":1,"stacks":{`, ErrCorrupt},
		"no version":     {`{"stacks":{}}`, ErrCorrupt},
		"future version": {`{"version":2,"stacks":{}}`, ErrUnsupported},
		"unknown field":  {`{"version":1,"stacks":{},"extra":true}`, ErrCorrupt},
		"trailing data":  {`{"version":1,"stacks":{}} {}`, ErrCorrupt},
		"overlap": {`{"version":1,"stacks":{
			"a":{"name":"a","directory":"/a","allocations":[{"type":"fixed","start":80,"end":80,"protocol":"tcp","bind_address":"0.0.0.0"}],"assignments":[]},
			"b":{"name":"b","directory":"/b","allocations":[{"type":"fixed","start":80,"end":80,"protocol":"tcp","bind_address":"0.0.0.0"}],"assignments":[]}}}`, ErrCorrupt},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "registry.json")
			if err := os.WriteFile(p, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadRegistry(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if b, _ := os.ReadFile(p); string(b) != c.content {
				t.Fatal("bad registry was modified")
			}
		})
	}
}

func TestResolveStackDirSafety(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if dir, ok, err := ResolveStackDir(root, "real"); err != nil || !ok || dir != filepath.Join(root, "real") {
		t.Fatalf("real: %q %v %v", dir, ok, err)
	}
	if _, ok, err := ResolveStackDir(root, "missing"); err != nil || ok {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if _, _, err := ResolveStackDir(root, "evil"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlinked stack dir accepted: %v", err)
	}
	if _, _, err := ResolveStackDir(root, "file"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("file accepted as stack dir: %v", err)
	}
	for _, bad := range []string{"..", "../x", "a/b", "/etc"} {
		if _, _, err := ResolveStackDir(root, bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	// A symlinked stack root itself is allowed (administrator's choice).
	linkRoot := filepath.Join(t.TempDir(), "stacks")
	if err := os.Symlink(root, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ResolveStackDir(linkRoot, "real"); err != nil || !ok {
		t.Fatalf("symlinked root: %v %v", ok, err)
	}
}

func testManifest(dir string) *model.Manifest {
	return &model.Manifest{Version: 1, Stack: "app", Directory: dir,
		Allocations: []model.Allocation{{Type: "block", Start: 12000, End: 12004, Protocol: "both", BindAddress: "0.0.0.0"}},
		Assignments: []model.Assignment{}}
}

func TestManifestSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ManifestPath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(dir); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("read through symlink: %v", err)
	}
	if err := WriteManifest(dir, testManifest(dir), false); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("write through symlink: %v", err)
	}
	if err := RemoveManifest(dir); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("remove symlink: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Fatal("symlink target modified")
	}
}

func TestManifestCreateNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := WriteManifest(dir, testManifest(dir), true); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, testManifest(dir), true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("expected ErrExist, got %v", err)
	}
	m, err := ReadManifest(dir)
	if err != nil || m.Stack != "app" {
		t.Fatalf("%v %v", m, err)
	}
	bad := testManifest(dir)
	bad.Allocations[0].End = 70000
	if err := WriteManifest(dir, bad, false); err == nil {
		t.Fatal("invalid manifest written")
	}
}

func TestMoveAndCopyAside(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("data"), 0o644)
	c1, err := CopyAside(p, "bak", t0)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := CopyAside(p, "bak", t0)
	if err != nil || c1 == c2 {
		t.Fatalf("second copy must get a unique name: %q %q %v", c1, c2, err)
	}
	m, err := MoveAside(p, "deleted", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("original still present")
	}
	if b, _ := os.ReadFile(m); string(b) != "data" {
		t.Fatal("moved content differs")
	}
}

func TestLockExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	l1, err := AcquireLock(p, true)
	if err != nil {
		t.Fatal(err)
	}
	old := LockTimeout
	LockTimeout = 100 * time.Millisecond
	defer func() { LockTimeout = old }()
	if _, err := AcquireLock(p, true); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("second exclusive lock: %v", err)
	}
	if _, err := AcquireLock(p, false); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("shared lock while exclusive held: %v", err)
	}
	l1.Release()
	s1, err := AcquireLock(p, false)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := AcquireLock(p, false)
	if err != nil {
		t.Fatalf("shared locks must coexist: %v", err)
	}
	s1.Release()
	s2.Release()
}

func TestLockSerializesWriters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "counter")
	os.WriteFile(p, []byte("0"), 0o644)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := AcquireLock(p, true)
			if err != nil {
				t.Error(err)
				return
			}
			defer l.Release()
			b, _ := os.ReadFile(p)
			n := len(b)
			time.Sleep(time.Millisecond)
			_ = WriteFileAtomic(p, []byte(strings.Repeat("x", n+1)), 0o644, false)
		}()
	}
	wg.Wait()
	if b, _ := os.ReadFile(p); len(b) != 21 {
		t.Fatalf("lost updates: length %d, want 21", len(b))
	}
}

func TestLockRequiresRegistryDir(t *testing.T) {
	if _, err := AcquireLock(filepath.Join(t.TempDir(), "nope", "registry.json"), true); !errors.Is(err, ErrRegistryNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestScanManifestsSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	os.WriteFile(ManifestPath(other), []byte("{}"), 0o644)
	os.Mkdir(filepath.Join(root, "a"), 0o755)
	os.WriteFile(ManifestPath(filepath.Join(root, "a")), []byte("{}"), 0o644)
	os.Mkdir(filepath.Join(root, "b"), 0o755)
	os.Symlink(other, filepath.Join(root, "link"))
	names, err := ScanManifests(root)
	if err != nil || len(names) != 1 || names[0] != "a" {
		t.Fatalf("got %v %v", names, err)
	}
}
