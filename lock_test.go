package challenge

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockRefusesASecondHolder(t *testing.T) {
	h := testHome(t)

	first, err := acquireLock(h.lockPath)
	require.NoError(t, err)

	_, err = acquireLock(h.lockPath)
	assert.ErrorIs(t, err, errLocked, "a concurrent run is a loud refusal, never a second world")

	require.NoError(t, first.release())

	second, err := acquireLock(h.lockPath)
	require.NoError(t, err)
	require.NoError(t, second.release())
}

func TestLockSurvivesEveryLifecycleOperation(t *testing.T) {
	h := testHome(t)
	held, err := acquireLock(h.lockPath)
	require.NoError(t, err)
	defer held.release()

	corridor := []corridorEntry{{Name: "estate"}}
	_, err = h.reset(corridor, "r_test")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(h.world()+"/state", []byte("x"), 0o644))

	// each of these mutates the tree the lock guards. none of them may unlink the
	// inode being held: doing so would leave the old inode locked while a second
	// process locks a fresh one at the same path, defeating the guard exactly
	// during the destructive operations it exists to make safe.
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"navigate", func() error { _, err := h.navigate(corridor, 1, resumeReplay); return err }},
		{"reset", func() error { _, err := h.reset(corridor, "r_test"); return err }},
		{"clean", h.clean},
	} {
		require.NoError(t, op.run(), op.name)
		_, err := acquireLock(h.lockPath)
		assert.ErrorIs(t, err, errLocked, "the lock must still refuse after %s", op.name)
	}
}
