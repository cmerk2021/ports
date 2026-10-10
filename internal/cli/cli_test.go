package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cmerk2021/ports/internal/model"
	"github.com/cmerk2021/ports/internal/store"
)

// fakeDocker emulates `docker compose ... config --format json` by printing
// $PWD/fake-compose.json; fake-stderr and fake-exit customise failures.
const fakeDocker = `#!/bin/sh
[ -f fake-stderr ] && cat fake-stderr >&2
if [ -f fake-exit ]; then exit "$(cat fake-exit)"; fi
cat fake-compose.json
`

type harness struct {
	t      *testing.T
	base   string
	root   string
	config string
}

func newHarness(t *testing.T, docker bool) *harness {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unix only")
	}
	base := t.TempDir()
	h := &harness{t: t, base: base, root: filepath.Join(base, "stacks"), config: filepath.Join(base, "config.json")}
	if err := os.Mkdir(h.root, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerPath := filepath.Join(base, "no-such-docker")
	if docker {
		dockerPath = filepath.Join(base, "docker")
		if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{
		"version":        1,
		"stack_root":     h.root,
		"registry_path":  filepath.Join(base, "state", "registry.json"),
		"block_size":     5,
		"dynamic_range":  map[string]int{"start": 12000, "end": 29999},
		"docker_command": dockerPath,
		"proc_net_dir":   filepath.Join(base, "proc"),
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(h.config, b, 0o644); err != nil {
		t.Fatal(err)
	}
	h.ok("init")
	return h
}

func (h *harness) run(args ...string) (int, string, string) {
	h.t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), append([]string{"--config", h.config}, args...), strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	code, out, errs := h.run(args...)
	if code != 0 {
		h.t.Fatalf("ports %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out
}

func (h *harness) expect(want int, args ...string) (string, string) {
	h.t.Helper()
	code, out, errs := h.run(args...)
	if code != want {
		h.t.Fatalf("ports %v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errs)
	}
	return out, errs
}

func (h *harness) stackDir(name string) string {
	d := filepath.Join(h.root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) compose(name, js string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.stackDir(name), "fake-compose.json"), []byte(js), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func TestDefinitionOfDoneWorkflow(t *testing.T) {
	h := newHarness(t, true)
	h.stackDir("jellyfin")
	out := h.ok("new", "jellyfin", "--fixed", "8096")
	if !strings.Contains(out, "12000-12004") || !strings.Contains(out, "8096") {
		t.Fatalf("new output: %s", out)
	}
	m, err := store.ReadManifest(filepath.Join(h.root, "jellyfin"))
	if err != nil || len(m.Allocations) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	h.ok("assign", "jellyfin", "web", "8096", "--purpose", "web UI")

	h.compose("jellyfin", `{"name":"jellyfin","services":{"jellyfin":{"ports":[{"mode":"ingress","target":8096,"published":"8096","protocol":"tcp"}]}}}`)
	out = h.ok("jellyfin")
	for _, want := range []string{"jellyfin", "8096", "12000-12004", "web"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q: %s", want, out)
		}
	}
	out = h.ok("find", "8096")
	if !strings.Contains(out, "jellyfin") {
		t.Fatalf("find: %s", out)
	}
	h.expect(ExitNotFound, "find", "8096", "--protocol", "udp")
	h.ok("check", "jellyfin")
	h.ok("audit")

	// List JSON output is machine readable.
	var list struct {
		Stacks []model.Stack `json:"stacks"`
	}
	if err := json.Unmarshal([]byte(h.ok("list", "--json")), &list); err != nil || len(list.Stacks) != 1 || len(list.Stacks[0].Assignments) != 1 {
		t.Fatalf("list --json: %v %v", list, err)
	}
	var show map[string]any
	if err := json.Unmarshal([]byte(h.ok("jellyfin", "--json")), &show); err != nil {
		t.Fatalf("show --json: %v", err)
	}
}

func TestAuditExitCodes(t *testing.T) {
	h := newHarness(t, true)
	h.stackDir("app")
	h.ok("new", "app")
	h.ok("assign", "app", "web", "80")

	// Published port outside the stack's reservations → violation.
	h.compose("app", `{"services":{"web":{"ports":[{"target":80,"published":"8080"}]}}}`)
	out, _ := h.expect(ExitConflict, "check", "app", "--no-host")
	if !strings.Contains(out, "outside") {
		t.Fatalf("check output: %s", out)
	}
	var rep map[string]any
	out, _ = h.expect(ExitConflict, "audit", "--json", "--no-host")
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("audit --json not JSON: %v\n%s", err, out)
	}

	// Malformed compose → configuration error, distinct exit code.
	dir := h.stackDir("app")
	os.WriteFile(filepath.Join(dir, "fake-stderr"), []byte("yaml: line 3: did not find expected key\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "fake-exit"), []byte("1"), 0o644)
	out, _ = h.expect(ExitAuditConfig, "check", "app", "--no-host")
	if !strings.Contains(out, "did not find expected key") {
		t.Fatalf("config error not reported: %s", out)
	}
	os.Remove(filepath.Join(dir, "fake-exit"))

	// Unresolved variable warning is reported.
	os.WriteFile(filepath.Join(dir, "fake-stderr"), []byte(`time="x" level=warning msg="The \"WEB_PORT\" variable is not set. Defaulting to a blank string."`+"\n"), 0o644)
	h.compose("app", `{"services":{"web":{"ports":[{"target":80,"published":"12000"}]}}}`)
	out, _ = h.expect(ExitAuditConfig, "check", "app", "--no-host")
	if !strings.Contains(out, "WEB_PORT") {
		t.Fatalf("unresolved variable not reported: %s", out)
	}
	os.Remove(filepath.Join(dir, "fake-stderr"))
	h.ok("check", "app", "--no-host")
}

func TestDockerUnavailable(t *testing.T) {
	h := newHarness(t, false)
	h.stackDir("app")
	h.ok("new", "app", "--fixed", "443")
	out := h.ok("app")
	if !strings.Contains(out, "443") {
		t.Fatalf("lookup without docker: %s", out)
	}
	h.ok("list")
	h.ok("find", "443")
	out, _ = h.expect(ExitAuditConfig, "check", "app")
	if !strings.Contains(strings.ToLower(out), "docker") {
		t.Fatalf("expected docker unavailable message: %s", out)
	}
}

func TestUsageAndErrorExitCodes(t *testing.T) {
	h := newHarness(t, false)
	h.expect(ExitUsage)
	h.ok("--help")
	h.ok("help", "new")
	h.ok("new", "--help")
	h.expect(ExitUsage, "new")
	h.expect(ExitUsage, "new", "../x", "--init-dir")
	if _, errs := h.expect(ExitNotFound, "8096"); !strings.Contains(errs, "ports find 8096") {
		t.Fatalf("missing hint: %s", errs)
	}
	h.expect(ExitUsage, "new", "x", "--fixed", "70000", "--init-dir")
	h.expect(ExitUsage, "new", "x", "--bogus")
	h.expect(ExitNotFound, "nosuchstack")
	h.expect(ExitNotFound, "new", "nodir")
	h.ok("new", "a", "--init-dir", "--fixed", "80")
	h.expect(ExitUsage, "new", "b", "--init-dir", "--block-size", "10", "--no-block", "--fixed", "53/udp")
	h.ok("new", "b", "--init-dir", "--no-block", "--fixed", "53/udp")
	if out := h.ok("new", "big", "--init-dir", "--block-size", "10"); !strings.Contains(out, "12005-12014") {
		t.Fatalf("block size override: %s", out)
	}
	h.expect(ExitConflict, "new", "a", "--init-dir")
	h.expect(ExitConflict, "new", "c", "--init-dir", "--fixed", "80")

	// --json errors are machine readable.
	out, _ := h.expect(ExitConflict, "new", "c", "--init-dir", "--fixed", "80", "--json")
	var e struct {
		Error    string `json:"error"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil || e.ExitCode != ExitConflict || e.Error == "" {
		t.Fatalf("json error: %q %v", out, err)
	}

	// Destructive operations require confirmation (stdin is not a terminal).
	h.expect(ExitError, "delete", "a", "--release-ports")
	h.expect(ExitError, "delete", "a")
	h.ok("delete", "a", "--release-ports", "--yes")
	h.expect(ExitNotFound, "a")
}

func TestCorruptRegistryAndInconsistentManifest(t *testing.T) {
	h := newHarness(t, false)
	dir := h.stackDir("app")
	h.ok("new", "app")
	// Inconsistent manifest blocks mutations and sync repairs it.
	os.WriteFile(store.ManifestPath(dir), []byte("garbage"), 0o644)
	h.expect(ExitInconsistent, "assign", "app", "web", "80")
	h.expect(ExitInconsistent, "sync")
	h.expect(ExitInconsistent, "sync", "--dry-run")
	h.ok("sync", "--apply")
	h.ok("sync")
	h.ok("assign", "app", "web", "80")

	reg := filepath.Join(h.base, "state", "registry.json")
	os.WriteFile(reg, []byte("{"), 0o644)
	h.expect(ExitError, "list")
	h.expect(ExitError, "init")
	if b, _ := os.ReadFile(reg); string(b) != "{" {
		t.Fatal("corrupt registry overwritten")
	}
	h.ok("init", "--recover")
	out := h.ok("app")
	if !strings.Contains(out, "web") {
		t.Fatalf("assignment lost in recovery: %s", out)
	}
}

// TestConcurrentProcesses builds the real binary and runs many `ports new`
// processes in parallel against the same registry.
func TestConcurrentProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	h := newHarness(t, false)
	bin := filepath.Join(h.base, "ports")
	build := exec.Command("go", "build", "-o", bin, "github.com/cmerk2021/ports/cmd/ports")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build binary: %v\n%s", err, out)
	}
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(bin, "--config", h.config, "new", fmt.Sprintf("s%02d", i), "--init-dir")
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("s%02d: %v: %s", i, err, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	r, err := store.LoadRegistry(filepath.Join(h.base, "state", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Stacks) != n {
		t.Fatalf("%d stacks", len(r.Stacks))
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("overlapping allocations: %v", err)
	}
	starts := map[int]bool{}
	for _, s := range r.Stacks {
		for _, a := range s.Allocations {
			if a.Type == model.TypeBlock {
				if starts[a.Start] {
					t.Fatalf("duplicate block %v", a)
				}
				starts[a.Start] = true
			}
		}
	}
}
