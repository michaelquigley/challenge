package challenge

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testHome builds an isolated gauntlet tree for one test, and makes sure the
// tree's own read-only nodes cannot defeat the temp-directory cleanup.
func testHome(t *testing.T) *home {
	t.Helper()
	base := t.TempDir()
	t.Cleanup(func() { openTree(base) })
	h, err := newHome(base, "toy")
	require.NoError(t, err)
	require.NoError(t, h.ensure())
	return h
}

// testW opens a world handle over a fresh tree, focused on one challenge record.
func testW(t *testing.T) (*W, *ChallengeRun, *home) {
	t.Helper()
	h := testHome(t)
	run := &Run{Gauntlet: "toy", RunId: newId("r")}
	cur := &ChallengeRun{Name: "probe", Status: StatusExecuted}
	run.Challenges = append(run.Challenges, cur)
	w, err := newW(h, run, cur)
	require.NoError(t, err)
	return w, cur, h
}

// capture invokes fn the way the engine does, recovering the unwind a terminal
// finding panics with, and reports whether one arrived and which class it carried.
func capture(fn func()) (unwound bool, class FindingClass) {
	defer func() {
		if r := recover(); r != nil {
			u, ok := r.(unwind)
			if !ok {
				panic(r)
			}
			unwound, class = true, u.class
		}
	}()
	fn()
	return false, ClassAssertion
}

// openTree makes every directory beneath root writable again, so a test that
// deliberately produced a read-only world does not leave one behind.
func openTree(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

// writeFile writes a file with an exact mode and modification time, so the copy
// contract can be checked against something specific rather than something
// incidental.
func writeFile(t *testing.T, path, content string, mode fs.FileMode, mod time.Time) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	require.NoError(t, os.Chtimes(path, mod, mod))
}

// makeDir creates a directory with an exact mode and modification time.
func makeDir(t *testing.T, path string, mode fs.FileMode, mod time.Time) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755))
	require.NoError(t, os.Chmod(path, mode))
	require.NoError(t, os.Chtimes(path, mod, mod))
}
