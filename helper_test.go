package challenge

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

// testW opens a world handle over a fresh tree with the toy installed, focused on
// one challenge record.
func testW(t *testing.T) (*W, *ChallengeRun, *home) {
	t.Helper()
	h := testHome(t)
	installToy(t, h)
	run := &Run{Gauntlet: "toy", RunId: newId("r")}
	cur := &ChallengeRun{Name: "probe", Status: StatusExecuted}
	run.Challenges = append(run.Challenges, cur)
	w, err := newW(context.Background(), h, run, cur)
	require.NoError(t, err)
	// nothing supervised outlives the test that started it.
	t.Cleanup(func() { w.shutdown() })
	return w, cur, h
}

var (
	toyOnce sync.Once
	toyPath string
	toyErr  error
)

// toyBinary builds the toy product once for the whole test binary.
func toyBinary(t *testing.T) string {
	t.Helper()
	toyOnce.Do(func() {
		dir, err := os.MkdirTemp("", "challenge-toy-")
		if err != nil {
			toyErr = err
			return
		}
		toyPath = filepath.Join(dir, "toy")
		out, err := exec.Command("go", "build", "-o", toyPath, "./internal/toy").CombinedOutput()
		if err != nil {
			toyErr = fmt.Errorf("building the toy: %v: %s", err, out)
		}
	})
	require.NoError(t, toyErr)
	return toyPath
}

// installToy puts the toy in a world's bin/, the way a consumer's bootstrap hook
// produces the binary under test — beside the world, never inside it.
func installToy(t *testing.T, h *home) {
	t.Helper()
	data, err := os.ReadFile(toyBinary(t))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(h.bin(), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.bin(), "toy"), data, 0o755))
}

// freePort borrows a port the way the harness does, for tests that need one before
// a world exists.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serveFixture declares a supervised toy server on a free port.
func serveFixture(t *testing.T, name string, extra ...string) (Fixture, int) {
	t.Helper()
	port := freePort(t)
	literal := fmt.Sprintf("toy serve --port %d %s", port, strings.Join(extra, " "))
	return Fixture{
		Name:         name,
		Literal:      literal,
		BaseURL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		ReadyURL:     "/api/v1/config",
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  10 * time.Second,
	}, port
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

// unixGetpgid reports a process's group, for the pin that harness children are
// isolated from the runner's foreground group.
func unixGetpgid(pid int) (int, error) {
	return unix.Getpgid(pid)
}
