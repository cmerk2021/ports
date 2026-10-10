// Package store implements safe persistence for the global registry and the
// per-stack manifests: strict JSON decoding, schema-version checks, advisory
// locking, atomic writes and symlink-safe manifest handling.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/cmerk2021/ports/internal/model"
)

// maxFileSize bounds registry/manifest reads.
const maxFileSize = 16 << 20

// Sentinel errors.
var (
	ErrRegistryNotFound = errors.New("registry not found")
	ErrManifestNotFound = errors.New("manifest not found")
	ErrCorrupt          = errors.New("file is corrupt")
	ErrUnsupported      = errors.New("unsupported schema version")
	ErrUnsafePath       = errors.New("unsafe path")
)

// readLimited reads a regular file without following a final symlink.
func readLimited(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnsafePath, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrCorrupt, path, maxFileSize)
	}
	return data, nil
}

// decodeVersioned checks the schema version and then strictly decodes data
// into v. Unknown fields are rejected so that a rewrite can never silently
// drop data written by a newer or hand-edited file.
func decodeVersioned(path string, data []byte, v any) error {
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("%w: %s: invalid JSON: %v", ErrCorrupt, path, err)
	}
	if probe.Version == nil {
		return fmt.Errorf("%w: %s: missing \"version\" field", ErrCorrupt, path)
	}
	if *probe.Version != model.SchemaVersion {
		return fmt.Errorf("%w: %s has schema version %d but this binary supports version %d; refusing to read or modify it", ErrUnsupported, path, *probe.Version, model.SchemaVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCorrupt, path, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: %s: trailing data after JSON object", ErrCorrupt, path)
	}
	return nil
}

func encode(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// --- registry ---------------------------------------------------------------

// LoadRegistry reads and fully validates the registry.
func LoadRegistry(path string) (*model.Registry, error) {
	data, err := readLimited(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s (run `ports init` to create it)", ErrRegistryNotFound, path)
		}
		return nil, fmt.Errorf("cannot read registry %s: %w", path, err)
	}
	reg := &model.Registry{}
	if err := decodeVersioned(path, data, reg); err != nil {
		return nil, fmt.Errorf("%w (the registry was not modified; repair it by hand or run `ports init --recover`)", err)
	}
	if reg.Stacks == nil {
		reg.Stacks = map[string]*model.Stack{}
	}
	if err := reg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: registry %s is inconsistent: %v (the registry was not modified)", ErrCorrupt, path, err)
	}
	return reg, nil
}

// SaveRegistry validates and atomically replaces the registry.
func SaveRegistry(path string, reg *model.Registry) error {
	for _, s := range reg.Stacks {
		if s != nil {
			s.Normalize()
		}
	}
	if err := reg.Validate(); err != nil {
		return fmt.Errorf("refusing to write invalid registry: %w", err)
	}
	data, err := encode(reg)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644, false)
}

// CreateRegistry writes a new empty registry, creating the parent directory.
// It never overwrites an existing file.
func CreateRegistry(path string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot create registry directory: %w", err)
	}
	data, err := encode(model.NewRegistry(now))
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644, true)
}

// --- stack directories and manifests ----------------------------------------

// ResolveStackDir returns the stack directory for name under root after
// checking that the name is valid, the directory is a real directory (not a
// symlink) and that its resolved location lies directly inside the resolved
// root. exists is false if the directory does not exist (dir is still
// returned so callers can create it).
func ResolveStackDir(root, name string) (dir string, exists bool, err error) {
	if err := model.ValidateStackName(name); err != nil {
		return "", false, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, fmt.Errorf("stack root %s is not accessible: %w", root, err)
	}
	if fi, err := os.Stat(realRoot); err != nil || !fi.IsDir() {
		return "", false, fmt.Errorf("stack root %s is not a directory", root)
	}
	dir = filepath.Join(root, name)
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return dir, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cannot inspect %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("%w: stack directory %s is a symlink; refusing to use it", ErrUnsafePath, dir)
	}
	if !fi.IsDir() {
		return "", false, fmt.Errorf("%w: %s exists but is not a directory", ErrUnsafePath, dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, fmt.Errorf("cannot resolve %s: %w", dir, err)
	}
	if filepath.Dir(real) != realRoot {
		return "", false, fmt.Errorf("%w: %s resolves outside the stack root %s", ErrUnsafePath, dir, root)
	}
	return dir, true, nil
}

// CreateStackDir creates the stack directory (mode 0755) if missing.
func CreateStackDir(root, name string) (string, error) {
	dir, exists, err := ResolveStackDir(root, name)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("cannot create stack directory %s: %w", dir, err)
		}
	}
	dir, _, err = ResolveStackDir(root, name)
	return dir, err
}

// ManifestPath returns the manifest path for a stack directory.
func ManifestPath(dir string) string { return filepath.Join(dir, ".ports") }

// checkManifestPath refuses symlinks and non-regular files at the manifest
// location. It returns false if nothing exists there.
// checkRealDir refuses to write into a stack directory that has been replaced
// by a symlink or a non-directory since it was registered.
func checkRealDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cannot inspect stack directory %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: stack directory %s is a symlink; refusing to write through it", ErrUnsafePath, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafePath, dir)
	}
	return nil
}

func checkManifestPath(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot inspect %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return true, fmt.Errorf("%w: manifest %s is a symlink; refusing to follow it", ErrUnsafePath, path)
	}
	if !fi.Mode().IsRegular() {
		return true, fmt.Errorf("%w: manifest %s is not a regular file", ErrUnsafePath, path)
	}
	return true, nil
}

// ReadManifest reads and validates a stack manifest.
func ReadManifest(dir string) (*model.Manifest, error) {
	path := ManifestPath(dir)
	exists, err := checkManifestPath(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrManifestNotFound, path)
	}
	data, err := readLimited(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrManifestNotFound, path)
		}
		return nil, fmt.Errorf("cannot read manifest %s: %w", path, err)
	}
	m := &model.Manifest{}
	if err := decodeVersioned(path, data, m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%w: manifest %s: %v", ErrCorrupt, path, err)
	}
	return m, nil
}

// ManifestExists reports whether something exists at the manifest path.
func ManifestExists(dir string) (bool, error) {
	_, err := os.Lstat(ManifestPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// WriteManifest atomically writes a manifest. With create set the write fails
// if any file already exists at the manifest path.
func WriteManifest(dir string, m *model.Manifest, create bool) error {
	path := ManifestPath(dir)
	if err := checkRealDir(dir); err != nil {
		return err
	}
	if _, err := checkManifestPath(path); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("refusing to write invalid manifest %s: %w", path, err)
	}
	data, err := encode(m)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644, create)
}

// RestoreManifestBytes puts raw previous manifest content back (rollback).
func RestoreManifestBytes(dir string, data []byte) error {
	if err := checkRealDir(dir); err != nil {
		return err
	}
	return WriteFileAtomic(ManifestPath(dir), data, 0o644, false)
}

// ReadManifestBytes returns the raw manifest content, or nil if absent.
func ReadManifestBytes(dir string) ([]byte, error) {
	path := ManifestPath(dir)
	exists, err := checkManifestPath(path)
	if err != nil || !exists {
		return nil, err
	}
	return readLimited(path)
}

// RemoveManifest deletes the manifest (used to roll back a creation).
func RemoveManifest(dir string) error {
	path := ManifestPath(dir)
	if _, err := checkManifestPath(path); err != nil {
		return err
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// MoveAside renames path to "<path>.<suffix>-<timestamp>" and returns the new
// name. It never overwrites an existing file.
func MoveAside(path, suffix string, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.%s-%s", path, suffix, now.UTC().Format("20060102T150405Z"))
	dest := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(dest); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dest = fmt.Sprintf("%s.%d", base, i)
	}
	if err := os.Link(path, dest); err != nil {
		return "", fmt.Errorf("cannot preserve %s as %s: %w", path, dest, err)
	}
	if err := os.Remove(path); err != nil {
		return dest, fmt.Errorf("preserved %s as %s but could not remove the original: %w", path, dest, err)
	}
	syncDir(filepath.Dir(path))
	return dest, nil
}

// CopyAside copies path to "<path>.<suffix>-<timestamp>" without modifying
// the original, returning the new name.
func CopyAside(path, suffix string, now time.Time) (string, error) {
	data, err := readLimited(path)
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("%s.%s-%s", path, suffix, now.UTC().Format("20060102T150405Z"))
	dest := base
	for i := 1; ; i++ {
		err := WriteFileAtomic(dest, data, 0o644, true)
		if err == nil {
			return dest, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		dest = fmt.Sprintf("%s.%d", base, i)
	}
}

// ScanManifests lists stack directories under root that contain a manifest
// file (or something at the manifest path). Symlinked directories are
// skipped.
func ScanManifests(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("cannot read stack root %s: %w", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if ok, _ := ManifestExists(dir); ok {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
