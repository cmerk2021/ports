package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// testHookBeforeCommit, when set by tests, is called just before an atomic
// write is committed (renamed/linked into place). Returning an error simulates
// a failure at the commit point.
var testHookBeforeCommit func(path string) error

// WriteFileAtomic writes data to path via a temporary file in the same
// directory, fsync and rename. If noReplace is set the commit uses link(2), so
// an existing file at path is never overwritten (the call fails with
// fs.ErrExist instead). On any failure the destination is left untouched and
// the temporary file is removed.
func WriteFileAtomic(path string, data []byte, perm os.FileMode, noReplace bool) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot write %s: %w", tmpName, err)
	}
	if err = tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot set permissions on %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot sync %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("cannot close %s: %w", tmpName, err)
	}
	if testHookBeforeCommit != nil {
		if err = testHookBeforeCommit(path); err != nil {
			return err
		}
	}
	if noReplace {
		if err = os.Link(tmpName, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("refusing to overwrite existing %s: %w", path, os.ErrExist)
			}
			return fmt.Errorf("cannot create %s: %w", path, err)
		}
		_ = os.Remove(tmpName)
	} else if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
