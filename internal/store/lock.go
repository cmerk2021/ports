package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LockTimeout bounds how long a command waits for another ports process.
var LockTimeout = 30 * time.Second

// ErrLockTimeout is returned when the registry lock cannot be acquired.
var ErrLockTimeout = errors.New("timed out waiting for the registry lock (another ports command may be running)")

// Lock is an advisory lock on the registry, held via flock(2) on a sidecar
// "<registry>.lock" file. The lock file itself is never removed, which avoids
// races between unlinking and re-locking.
type Lock struct {
	f *os.File
}

// LockPath returns the lock file path for a registry path.
func LockPath(registryPath string) string { return registryPath + ".lock" }

// AcquireLock takes a shared (read) or exclusive (write) lock. The registry
// directory must already exist (see `ports init`).
func AcquireLock(registryPath string, exclusive bool) (*Lock, error) {
	dir := filepath.Dir(registryPath)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: registry directory %s does not exist (run `ports init` first)", ErrRegistryNotFound, dir)
	}
	f, err := os.OpenFile(LockPath(registryPath), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		if !exclusive && errors.Is(err, os.ErrPermission) {
			// Read-only users may still read the registry; fall back to an
			// unlocked read of an atomically replaced file.
			return &Lock{}, nil
		}
		return nil, fmt.Errorf("cannot open registry lock file: %w", err)
	}
	deadline := time.Now().Add(LockTimeout)
	for {
		ok, err := tryLock(f, exclusive)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("cannot lock registry: %w", err)
		}
		if ok {
			return &Lock{f: f}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlock(l.f)
	_ = l.f.Close()
	l.f = nil
}
