// Package lock provides an exclusive file lock to serialize deploys and repo edits.
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Lock struct{ f *os.File }

// Acquire blocks until it holds an exclusive lock on path; what describes the
// holder in the message printed while waiting.
func Acquire(path, what string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "coolix: waiting for %s to finish (%s)\n", what, path)
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &Lock{f}, nil
}

func (l *Lock) Release() { l.f.Close() }
