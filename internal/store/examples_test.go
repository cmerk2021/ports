package store

import (
	"path/filepath"
	"testing"

	"github.com/cmerk2021/ports/internal/model"
)

// TestExamples keeps the documented example files valid.
func TestExamples(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join("..", "..", "examples", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.Abs(filepath.Join("..", "..", "examples", "stacks", "jellyfin"))
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d := model.DiffStackManifest(reg.Stacks["jellyfin"], m); len(d) > 0 {
		t.Fatalf("example manifest and registry disagree: %v", d)
	}
}
