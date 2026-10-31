// Package lock provides exclusive file locks that serialize deploys and repo
// edits.
package lock

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type Lock struct{ f *os.File }

// Acquire blocks until it holds an exclusive lock on path. While waiting it
// tells w (e.g. a job's log) that what is holding it.
func Acquire(path, what string, w io.Writer) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if w != nil {
			fmt.Fprintf(w, "coolix: waiting for %s to finish\n", what)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &Lock{f}, nil
}

func (l *Lock) Release() { l.f.Close() }
