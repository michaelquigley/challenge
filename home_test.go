package challenge

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHomeIsDeclaredNotDiscovered(t *testing.T) {
	_, err := newHome("relative/path", "toy")
	assert.Error(t, err, "a world home resolved from the working directory is a different world every time")

	h, err := newHome("/srv/suites/reef", "reef")
	require.NoError(t, err)
	assert.Equal(t, "/srv/suites/reef/.challenge/reef", h.root)
	assert.Equal(t, "/srv/suites/reef/.challenge/reef.lock", h.lockPath)
	assert.Equal(t, "/srv/suites/reef/.challenge/reef/world/.harness", h.harness())
}

func TestGauntletNamesAreValidatedNotSanitized(t *testing.T) {
	// sanitizing would let two differently-named gauntlets share one world in
	// silence, and let a name resolve the tree somewhere lifecycle operations have
	// no business touching. an unusable name is a broken harness input, so it says
	// so instead of normalizing into something plausible.
	for _, name := range []string{"", ".", "..", "reef/../..", "a/b", "-leading", "with space", "quotes\"here"} {
		_, err := newHome("/srv/suites", name)
		assert.Error(t, err, "gauntlet name %q", name)
	}
	for _, name := range []string{"reef", "flo-2", "reef.e2e", "toy_world"} {
		_, err := newHome("/srv/suites", name)
		assert.NoError(t, err, "gauntlet name %q", name)
	}
}

func TestResetMintsASessionAndSparesTheBinary(t *testing.T) {
	h := testHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.bin(), "reef"), []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "leftover"), []byte("residue"), 0o644))

	corridor := []corridorEntry{{Name: "estate"}, {Name: "containers", NoCheckpoint: true}}
	s, err := h.reset(corridor, "r_test")
	require.NoError(t, err)
	assert.NotEmpty(t, s.Id)
	assert.Equal(t, corridor, s.Corridor)

	_, err = os.Lstat(filepath.Join(h.world(), "leftover"))
	assert.ErrorIs(t, err, fs.ErrNotExist, "reset discards the world generation")

	// a world generation arrives with its own boundary zero, so the corridor's
	// first challenge resolves like any other rather than being a special case.
	refs, err := newCheckpoints(h.checkpointsDir()).list()
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, 0, refs[0].Boundary)
	assert.Equal(t, genesisName, refs[0].Name)

	// the bootstrap that produced the binary has already run by the time reset
	// does; the artifact it built is not world state.
	_, err = os.Lstat(filepath.Join(h.bin(), "reef"))
	assert.NoError(t, err)

	reread, err := loadSession(h.sessionPath())
	require.NoError(t, err)
	assert.Equal(t, s.Id, reread.Id)
	assert.Equal(t, corridor, reread.Corridor)
}

func TestCleanEmptiesTheTreeAndSparesTheLock(t *testing.T) {
	h := testHome(t)
	l, err := acquireLock(h.lockPath)
	require.NoError(t, err)
	defer l.release()

	before, err := os.Stat(h.lockPath)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "state"), []byte("x"), 0o644))
	require.NoError(t, h.clean())

	entries, err := os.ReadDir(h.root)
	require.NoError(t, err)
	assert.Empty(t, entries, "clean empties the tree")

	// the lock is anchored beside the tree, so the operations it guards can never
	// unlink the inode being held.
	after, err := os.Stat(h.lockPath)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
}

// buildChain publishes one save point per named boundary, each holding a marker
// naming the boundary it closes, so a restore can be told apart from a no-op.
func buildChain(t *testing.T, h *home, names ...string) []checkpointRef {
	t.Helper()
	s, err := loadSession(h.sessionPath())
	require.NoError(t, err)
	c := newCheckpoints(h.checkpointsDir())
	var refs []checkpointRef
	for i, name := range names {
		require.NoError(t, os.WriteFile(filepath.Join(h.world(), "boundary"), []byte(name), 0o644))
		ref, err := c.publish(i, name, s.Id, "r_test", h.world())
		require.NoError(t, err)
		refs = append(refs, ref)
	}
	return refs
}

// toyCorridor is a four-challenge corridor whose boundaries buildChain publishes.
var toyCorridor = []corridorEntry{{Name: "estate"}, {Name: "containers"}, {Name: "slices"}, {Name: "durability"}}

func TestNavigationRestoresPrunesAndRebasesTogether(t *testing.T) {
	h := testHome(t)
	_, err := h.reset(toyCorridor, "r_test")
	require.NoError(t, err)
	buildChain(t, h, genesisName, "estate", "containers", "slices")

	// resuming challenge 2 restores the boundary strictly before it.
	ref, err := h.navigate(toyCorridor, 2, resumeReplay)
	require.NoError(t, err)
	assert.Equal(t, 1, ref.Boundary)

	body, err := os.ReadFile(filepath.Join(h.world(), "boundary"))
	require.NoError(t, err)
	assert.Equal(t, "estate", string(body), "the world moved back")

	remaining, err := newCheckpoints(h.checkpointsDir()).list()
	require.NoError(t, err)
	require.Len(t, remaining, 2, "the abandoned branch's save points must not remain selectable")
	assert.Equal(t, 0, remaining[0].Boundary)
	assert.Equal(t, 1, remaining[1].Boundary)
	assert.NoError(t, h.requireCompletePrune())
}

func TestNavigationValidatesTheCorridorBeforeItRestores(t *testing.T) {
	h := testHome(t)
	_, err := h.reset(toyCorridor, "r_test")
	require.NoError(t, err)
	buildChain(t, h, genesisName, "estate", "containers", "slices")

	// a challenge renamed before the target means the old boundaries describe a
	// narrative that no longer exists.
	diverged := []corridorEntry{{Name: "estate"}, {Name: "cabinets"}, {Name: "slices"}, {Name: "durability"}}
	_, err = h.navigate(diverged, 3, resumeReplay)
	require.ErrorIs(t, err, errDivergentCorridor)

	// and the refusal happened before anything moved.
	body, err := os.ReadFile(filepath.Join(h.world(), "boundary"))
	require.NoError(t, err)
	assert.Equal(t, "slices", string(body), "a refused navigation leaves the world alone")
	remaining, err := newCheckpoints(h.checkpointsDir()).list()
	require.NoError(t, err)
	assert.Len(t, remaining, 4)

	// a changed suffix is a legitimate branch, and the session rebases onto it so
	// the next navigation validates against the corridor that now exists.
	branched := []corridorEntry{{Name: "estate"}, {Name: "containers"}, {Name: "recovery"}, {Name: "drill"}}
	_, err = h.navigate(branched, 2, resumeReplay)
	require.NoError(t, err)

	s, err := loadSession(h.sessionPath())
	require.NoError(t, err)
	assert.Equal(t, branched, s.Corridor)
	_, err = h.navigate(branched, 3, resumeReplay)
	assert.NoError(t, err, "the rebase is what makes the branch resumable in turn")
}

func TestExactResumeRefusesWithoutItsPredecessor(t *testing.T) {
	h := testHome(t)
	_, err := h.reset(toyCorridor, "r_test")
	require.NoError(t, err)
	buildChain(t, h, genesisName, "estate")

	ref, err := h.navigate(toyCorridor, 2, resumeExact)
	require.NoError(t, err)
	assert.Equal(t, 1, ref.Boundary)

	// challenge 3's predecessor never published, and nothing else will do: running
	// one challenge alone means running it against exactly the world it inherits.
	_, err = h.navigate(toyCorridor, 3, resumeExact)
	assert.ErrorIs(t, err, errNoSavePoint)
}

func TestNavigationFailsClosed(t *testing.T) {
	h := testHome(t)
	_, err := h.reset(toyCorridor, "r_test")
	require.NoError(t, err)
	buildChain(t, h, genesisName, "estate", "containers", "slices")

	// the dangerous state exactly: the world has moved back, and the removal of
	// the future it abandoned cannot finish. there is no honest way to tell from
	// the outside how much survived, so the record stays set.
	require.NoError(t, os.Chmod(h.checkpointsDir(), 0o555))
	_, err = h.navigate(toyCorridor, 2, resumeReplay)
	require.Error(t, err)
	require.NoError(t, os.Chmod(h.checkpointsDir(), 0o755))

	// the prune reached one save point and could not finish it: the directory
	// survives with its manifest gone. a chain holding something that cannot say
	// what it is refuses to be read at all, which is the strongest form of the
	// guarantee — the abandoned future is not merely unselectable, it is
	// unresolvable.
	_, err = newCheckpoints(h.checkpointsDir()).list()
	require.Error(t, err, "the abandoned future did in fact survive this failure")

	rs, err := loadRunState(h.runStatePath())
	require.NoError(t, err)
	assert.True(t, rs.Pruning)
	assert.Equal(t, 1, rs.PruningTo)

	// the guard lives inside the operation, so a second attempt cannot walk past
	// it into one of the checkpoints that survived.
	_, err = h.navigate(toyCorridor, 4, resumeReplay)
	assert.ErrorIs(t, err, errIncompletePrune)
	body, err := os.ReadFile(filepath.Join(h.world(), "boundary"))
	require.NoError(t, err)
	assert.Equal(t, "estate", string(body), "the refused navigation touched nothing")

	// the recorded way forward actually works: cleaning the world discards the
	// generation whose future could not be accounted for, and the next reset
	// starts one whose record is whole.
	require.NoError(t, h.clean())
	_, err = h.reset(toyCorridor, "r_test")
	require.NoError(t, err)
	assert.NoError(t, h.requireCompletePrune())
}

func TestASavePointCarriesItsOwnIdentity(t *testing.T) {
	h := testHome(t)
	_, err := h.reset(toyCorridor, "r_test")
	require.NoError(t, err)
	buildChain(t, h, genesisName, "estate", "containers")

	// a directory name is a label, and a label can be changed independently of what
	// it labels. filing one boundary's world under another's name would restore a
	// world nobody closed at that coordinate — so the answer comes from inside the
	// save point rather than from the directory holding it.
	require.NoError(t, removeTree(filepath.Join(h.checkpointsDir(), "01-estate")))
	require.NoError(t, os.MkdirAll(filepath.Join(h.checkpointsDir(), "01-estate"), 0o755))
	require.NoError(t, copyTree(
		filepath.Join(h.checkpointsDir(), "02-containers"),
		filepath.Join(h.checkpointsDir(), "01-estate")))

	_, err = h.navigate(toyCorridor, 2, resumeReplay)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "published as boundary 2")
	assert.Contains(t, err.Error(), "clean the world")

	// and the world it would have restored was left alone.
	body, err := os.ReadFile(filepath.Join(h.world(), "boundary"))
	require.NoError(t, err)
	assert.Equal(t, "containers", string(body))
}
