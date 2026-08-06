package challenge

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// buildWorld lays down a tree exercising every shape the copy contract claims to
// carry: nested directories, exact modes and modification times, an empty
// directory, a symlink, a read-only file, and a read-only directory holding a
// file. reef's dying-drive drill produces the last of those for real.
func buildWorld(t *testing.T, root string) {
	t.Helper()
	base := time.Date(2019, 3, 14, 9, 26, 53, 589793000, time.UTC)

	writeFile(t, filepath.Join(root, "ws", "config.yaml"), "bind: 127.0.0.1:9000\n", 0o640, base)
	writeFile(t, filepath.Join(root, "nas", "album", "track.wav"), strings.Repeat("audio", 4096), 0o644, base.Add(time.Hour))
	writeFile(t, filepath.Join(root, "nas", "album", "notes.txt"), "provenance", 0o444, base.Add(2*time.Hour))
	makeDir(t, filepath.Join(root, "usb2"), 0o755, base.Add(3*time.Hour))
	link := filepath.Join(root, "ws", "latest.wav")
	require.NoError(t, os.Symlink("../nas/album/track.wav", link))
	require.NoError(t, lchtimes(link, base.Add(11*time.Hour)))

	// special mode bits change what the filesystem does, not merely who may read,
	// so they are part of the contract too.
	writeFile(t, filepath.Join(root, "ws", "helper"), "#!/bin/sh\n", 0o755|fs.ModeSetuid, base.Add(4*time.Hour))
	makeDir(t, filepath.Join(root, "usb"), 0o755|fs.ModeSetgid, base.Add(5*time.Hour))
	makeDir(t, filepath.Join(root, "nas"), 0o755|fs.ModeSticky, base.Add(6*time.Hour))

	// the read-only directory is created last: its own children have to land
	// before the mode does.
	roDir := filepath.Join(root, "usb3", "dying")
	writeFile(t, filepath.Join(roDir, "member.yaml"), "id: m1\n", 0o644, base.Add(7*time.Hour))
	makeDir(t, roDir, 0o555, base.Add(8*time.Hour))
	makeDir(t, filepath.Join(root, "nas", "album"), 0o750, base.Add(9*time.Hour))
	makeDir(t, filepath.Join(root, "ws"), 0o755, base.Add(10*time.Hour))
}

// treeEntry is one node's observable identity for a fidelity comparison.
type treeEntry struct {
	rel     string
	mode    fs.FileMode
	mod     time.Time
	link    string
	content string
}

// scanTree records every node under root the way a byte-comparer, a permission
// check, and a timestamp-aware scanner would each see it.
func scanTree(t *testing.T, root string) []treeEntry {
	t.Helper()
	var out []treeEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, err := filepath.Rel(root, p)
		require.NoError(t, err)
		info, err := d.Info()
		require.NoError(t, err)
		e := treeEntry{rel: rel, mode: info.Mode(), mod: info.ModTime()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			require.NoError(t, err)
			e.link = link
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			e.content = string(data)
		}
		out = append(out, e)
		return nil
	})
	require.NoError(t, err)
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

func TestCopyTreeIsFaithful(t *testing.T) {
	base := t.TempDir()
	t.Cleanup(func() { openTree(base) })
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	require.NoError(t, os.MkdirAll(src, 0o755))
	buildWorld(t, src)

	require.NoError(t, os.MkdirAll(dst, 0o755))
	require.NoError(t, copyTree(src, dst))

	before := scanTree(t, src)
	after := scanTree(t, dst)

	// the comparison is only worth anything if it actually reached every shape.
	var rels []string
	for _, e := range before {
		rels = append(rels, e.rel)
	}
	assert.Equal(t, []string{
		".", "nas", "nas/album", "nas/album/notes.txt", "nas/album/track.wav",
		"usb", "usb2", "usb3", "usb3/dying", "usb3/dying/member.yaml",
		"ws", "ws/config.yaml", "ws/helper", "ws/latest.wav",
	}, rels)

	// the special bits survived, not just the permission bits.
	for _, c := range []struct {
		rel string
		bit fs.FileMode
	}{{"ws/helper", fs.ModeSetuid}, {"usb", fs.ModeSetgid}, {"nas", fs.ModeSticky}} {
		info, err := os.Lstat(filepath.Join(dst, c.rel))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&c.bit, "%s lost its special mode bit", c.rel)
	}

	assert.Equal(t, before, after, "a restored world must be indistinguishable from the world that was saved")

	// the empty directory survived as a directory rather than vanishing.
	info, err := os.Lstat(filepath.Join(dst, "usb2"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// the symlink is a link, never the file it points at.
	info, err = os.Lstat(filepath.Join(dst, "ws", "latest.wav"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&fs.ModeSymlink)
}

func TestCopyTreeRefusesUnsupportedNodes(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, unix.Mkfifo(filepath.Join(src, "pipe"), 0o644))

	require.NoError(t, os.MkdirAll(dst, 0o755))
	err := copyTree(src, dst)
	require.ErrorIs(t, err, errUnsupportedNode, "a snapshot that cannot be faithful refuses to exist")
}

func TestPublishIsAtomic(t *testing.T) {
	h := testHome(t)
	buildWorld(t, h.world())
	c := newCheckpoints(h.checkpointsDir())

	ref, err := c.publish(0, genesisName, "r_test", h.world())
	require.NoError(t, err)
	assert.Equal(t, "00-genesis", filepath.Base(ref.Dir))

	// a world the contract cannot carry produces no checkpoint at all — not a
	// partial one, and not a directory a later resolver could reach.
	require.NoError(t, unix.Mkfifo(filepath.Join(h.world(), "pipe"), 0o644))
	_, err = c.publish(1, "durability", "r_test", h.world())
	require.ErrorIs(t, err, errUnsupportedNode)

	refs, err := c.list()
	require.NoError(t, err)
	require.Len(t, refs, 1, "the failed checkpoint must not be selectable")
	assert.Equal(t, 0, refs[0].Boundary)

	entries, err := os.ReadDir(h.checkpointsDir())
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), tmpPrefix), "a failed copy leaves nothing behind: %s", e.Name())
	}
}

func TestRepublishingABoundaryRetiresRatherThanErases(t *testing.T) {
	h := testHome(t)
	c := newCheckpoints(h.checkpointsDir())

	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "boundary"), []byte("first"), 0o644))
	_, err := c.publish(1, "estate", "r_one", h.world())
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "boundary"), []byte("second"), 0o644))
	ref, err := c.publish(1, "estate", "r_two", h.world())
	require.NoError(t, err)

	body, err := os.ReadFile(filepath.Join(ref.Dir, "boundary"))
	require.NoError(t, err)
	assert.Equal(t, "second", string(body))

	// nothing half-erased and nothing retired is left standing where a resolver
	// could reach it.
	refs, err := c.list()
	require.NoError(t, err)
	require.Len(t, refs, 1)
	entries, err := os.ReadDir(h.checkpointsDir())
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestListRefusesCorruptedCheckpointState(t *testing.T) {
	h := testHome(t)
	c := newCheckpoints(h.checkpointsDir())
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "state"), []byte("x"), 0o644))
	_, err := c.publish(0, genesisName, "r_test", h.world())
	require.NoError(t, err)

	// a save point replaced by a file is corrupted harness-owned state, and
	// silently skipping it would let navigation fall back to an earlier boundary
	// instead of saying the world cannot be trusted.
	require.NoError(t, os.WriteFile(filepath.Join(h.checkpointsDir(), "01-estate"), []byte("not a world"), 0o644))
	_, err = c.list()
	assert.Error(t, err)
	require.NoError(t, os.Remove(filepath.Join(h.checkpointsDir(), "01-estate")))

	// a boundary names exactly one closed world; two claimants would let the
	// exact-predecessor rule and the strictly-before rule restore different worlds
	// from the same coordinate.
	require.NoError(t, os.MkdirAll(filepath.Join(h.checkpointsDir(), "01-estate"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(h.checkpointsDir(), "01-cabinets"), 0o755))
	_, err = c.list()
	assert.Error(t, err)
}

func TestRestoreIsFaithfulAfterMutation(t *testing.T) {
	h := testHome(t)
	buildWorld(t, h.world())
	c := newCheckpoints(h.checkpointsDir())

	ref, err := c.publish(1, "durability", "r_test", h.world())
	require.NoError(t, err)
	saved := scanTree(t, h.world())

	// mutate every dimension the contract claims: bytes, mode bits, modification
	// times, and a symlink's target.
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "ws", "config.yaml"), []byte("bind: 0.0.0.0:1\n"), 0o600))
	require.NoError(t, os.Chmod(filepath.Join(h.world(), "nas", "album", "notes.txt"), 0o600))
	future := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(h.world(), "nas", "album", "track.wav"), future, future))
	require.NoError(t, os.Remove(filepath.Join(h.world(), "ws", "latest.wav")))
	require.NoError(t, os.Symlink("/dev/null", filepath.Join(h.world(), "ws", "latest.wav")))
	require.NoError(t, os.RemoveAll(filepath.Join(h.world(), "usb2")))

	require.NoError(t, c.restore(ref, h.world()))
	assert.Equal(t, saved, scanTree(t, h.world()))
}

func TestResumeResolvesStrictlyBeforeItsTarget(t *testing.T) {
	h := testHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "state"), []byte("x"), 0o644))
	c := newCheckpoints(h.checkpointsDir())

	for i, name := range []string{genesisName, "estate", "containers", "slices"} {
		_, err := c.publish(i, name, "r_test", h.world())
		require.NoError(t, err)
	}

	// the coordinate rule: reproducing challenge N restores the greatest boundary
	// before it, never its own aftermath.
	ref, ok, err := c.greatestBelow(3)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, ref.Boundary, "challenge 3 executes against challenge 2's world")

	// the fresh world is boundary zero, so the corridor's first challenge is no
	// special case.
	ref, ok, err = c.greatestBelow(1)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 0, ref.Boundary)
	assert.Equal(t, genesisName, ref.Name)

	// a challenge that opted out of checkpointing costs replay, not resolution.
	require.NoError(t, removeTree(filepath.Join(h.checkpointsDir(), "02-containers")))
	ref, ok, err = c.greatestBelow(3)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, ref.Boundary)
}

func TestRemoveAboveClearsTheAbandonedFuture(t *testing.T) {
	h := testHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "state"), []byte("x"), 0o644))
	c := newCheckpoints(h.checkpointsDir())
	for i, name := range []string{genesisName, "estate", "containers", "slices"} {
		_, err := c.publish(i, name, "r_test", h.world())
		require.NoError(t, err)
	}

	require.NoError(t, c.removeAbove(1))
	refs, err := c.list()
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, 0, refs[0].Boundary)
	assert.Equal(t, 1, refs[1].Boundary)
}

func TestRemoveTreeSurvivesAReadOnlyWorld(t *testing.T) {
	base := t.TempDir()
	t.Cleanup(func() { openTree(base) })
	root := filepath.Join(base, "world")
	writeFile(t, filepath.Join(root, "usb3", "dying", "member.yaml"), "id: m1\n", 0o644, time.Now())
	makeDir(t, filepath.Join(root, "usb3", "dying"), 0o555, time.Now())

	require.NoError(t, removeTree(root))
	_, err := os.Lstat(root)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}
