//go:build !unix

package store

import (
	"errors"
	"os"
)

const openNoFollow = 0

func tryLock(*os.File, bool) (bool, error) {
	return false, errors.New("file locking is only supported on Unix-like systems")
}

func unlock(*os.File) error { return nil }
