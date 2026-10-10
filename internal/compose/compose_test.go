package compose

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

const sample = `{
  "name": "ct",
  "services": {
    "h": {"image": "x", "network_mode": "host"},
    "web": {
      "image": "nginx",
      "ports": [
        {"mode": "ingress", "target": 80, "published": "8080", "protocol": "tcp"},
        {"mode": "ingress", "host_ip": "127.0.0.1", "target": 81, "published": "8081", "protocol": "udp"},
        {"mode": "ingress", "host_ip": "::1", "target": 82, "published": "8082", "protocol": "tcp"},
        {"mode": "ingress", "target": 9000, "published": "9000", "protocol": "tcp"},
        {"mode": "ingress", "target": 3000, "protocol": "tcp"},
        {"mode": "ingress", "target": 5000, "published": "5000-5001"},
        {"mode": "ingress", "target": 7000, "published": 7000}
      ]
    }
  }
}`

func TestParse(t *testing.T) {
	res, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := []Mapping{
		{"web", 8080, 80, "tcp", "0.0.0.0"},
		{"web", 8081, 81, "udp", "127.0.0.1"},
		{"web", 8082, 82, "tcp", "::1"},
		{"web", 9000, 9000, "tcp", "0.0.0.0"},
		{"web", 5000, 5000, "tcp", "0.0.0.0"},
		{"web", 5001, 5000, "tcp", "0.0.0.0"},
		{"web", 7000, 7000, "tcp", "0.0.0.0"},
	}
	if len(res.Mappings) != len(want) {
		t.Fatalf("got %v", res.Mappings)
	}
	for i := range want {
		if res.Mappings[i] != want[i] {
			t.Errorf("mapping %d: got %v want %v", i, res.Mappings[i], want[i])
		}
	}
	kinds := map[string]int{}
	for _, n := range res.Notes {
		kinds[n.Kind]++
	}
	if kinds[NoteHostNetwork] != 1 || kinds[NoteEphemeralPort] != 1 {
		t.Fatalf("notes = %v", res.Notes)
	}
}

func TestParseInvalid(t *testing.T) {
	for name, in := range map[string]string{
		"not json":     "nope",
		"bad target":   `{"services":{"a":{"ports":[{"target":"x","published":"80"}]}}}`,
		"bad port":     `{"services":{"a":{"ports":[{"target":80,"published":"70000"}]}}}`,
		"bad protocol": `{"services":{"a":{"ports":[{"target":80,"published":"80","protocol":"sctp"}]}}}`,
		"bad host ip":  `{"services":{"a":{"ports":[{"target":80,"published":"80","host_ip":"nope"}]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var ce *ConfigError
			if _, err := Parse([]byte(in)); !errors.As(err, &ce) {
				t.Fatalf("expected ConfigError, got %v", err)
			}
		})
	}
}

func TestUnresolvedVariables(t *testing.T) {
	stderr := []byte(`time="x" level=warning msg="The \"HTTPS_PORT\" variable is not set. Defaulting to a blank string."
time="x" level=warning msg="The \"A\" variable is not set. Defaulting to a blank string."
time="x" level=warning msg="The \"HTTPS_PORT\" variable is not set. Defaulting to a blank string."`)
	got := UnresolvedVariables(stderr)
	if len(got) != 2 || got[0] != "A" || got[1] != "HTTPS_PORT" {
		t.Fatalf("got %v", got)
	}
}

type fakeRunner struct {
	stdout, stderr string
	err            error
}

func (f fakeRunner) Config(context.Context, string, []string) ([]byte, []byte, error) {
	return []byte(f.stdout), []byte(f.stderr), f.err
}

func exitErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("false").Run()
	if err == nil {
		t.Skip("false(1) unavailable")
	}
	return err
}

func TestInspect(t *testing.T) {
	ctx := context.Background()
	res, err := Inspect(ctx, fakeRunner{stdout: sample, stderr: `level=warning msg="The \"X\" variable is not set."`}, "/d", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range res.Notes {
		found = found || n.Kind == NoteUnresolvedVariable
	}
	if !found {
		t.Fatal("unresolved variable not reported")
	}

	_, err = Inspect(ctx, fakeRunner{stderr: "yaml: line 1: did not find expected node content", err: exitErr(t)}, "/d", nil)
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Message != "yaml: line 1: did not find expected node content" {
		t.Fatalf("got %v", err)
	}

	_, err = Inspect(ctx, fakeRunner{stderr: "docker: 'compose' is not a docker command.", err: exitErr(t)}, "/d", nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing plugin: got %v", err)
	}
}

func TestExecRunnerMissingDocker(t *testing.T) {
	_, err := Inspect(context.Background(), ExecRunner{Command: "/nonexistent/docker"}, t.TempDir(), nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}
