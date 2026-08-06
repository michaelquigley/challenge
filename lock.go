package challenge

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// errLocked is what an invocation earns when another run already holds the
// gauntlet's world. the stable world path means a concurrent second run would
// collide with the first; the lock turns that collision into a loud refusal rather
// than a second world at a mangled path.
var errLocked = errors.New("world is locked by another run")

// lock is the exclusive guard on one gauntlet's world tree, held for an entire
// invocation.
//
// it is anchored beside the tree, never inside it. unlinking a locked file leaves
// the old inode locked while a second process creates and locks a fresh one at the
// same path, so a guard living inside the thing it guards is defeated by exactly
// the destructive operations — reset, restore, clean — it exists to make safe.
type lock struct {
	path string
	f    *os.File
}

// acquireLock takes the gauntlet's lock without blocking. every lifecycle
// operation acquires it first and mutates only the tree's children.
func acquireLock(path string) (*lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", errLocked, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return &lock{path: path, f: f}, nil
}

// release drops the lock. the lock file itself stays: it is the anchor, and
// unlinking it is what the anchoring rule forbids.
func (l *lock) release() error {
	if l.f == nil {
		return nil
	}
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("unlocking %s: %w", l.path, err)
	}
	if closeErr != nil {
		return fmt.Errorf("closing lock %s: %w", l.path, closeErr)
	}
	return nil
}
