package challenge

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// home resolves every path in one gauntlet's world tree.
//
// the tree lives at <worldHome>/.challenge/<gauntlet>/ and its lock sits beside
// it, outside every subtree that reset, restore, and clean mutate. the home is
// declared rather than discovered: products record absolute paths inside their own
// state, so a checkpoint restored anywhere but its original root is dead on
// arrival, and every face has to resolve the same tree regardless of the working
// directory it was invoked from.
type home struct {
	gauntlet string
	root     string
	lockPath string
}

// gauntletName is what a gauntlet may be called. the name becomes a directory and
// a lock file, so it is validated rather than normalized: sanitizing would let two
// differently-named gauntlets share one world in silence, and a name like ".."
// would resolve the tree somewhere the lifecycle operations have no business
// touching.
var gauntletName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// newHome resolves a gauntlet's tree beneath an absolute world home.
func newHome(worldHome, gauntlet string) (*home, error) {
	if gauntlet == "" {
		return nil, errors.New("a gauntlet needs a name")
	}
	if !gauntletName.MatchString(gauntlet) || gauntlet == "." || gauntlet == ".." {
		return nil, fmt.Errorf("gauntlet name %q must be a plain path component of letters, digits, and .-_ starting with a letter or digit", gauntlet)
	}
	if !filepath.IsAbs(worldHome) {
		return nil, fmt.Errorf("world home %q must be an absolute path", worldHome)
	}
	base := filepath.Join(filepath.Clean(worldHome), ".challenge")
	return &home{
		gauntlet: gauntlet,
		root:     filepath.Join(base, gauntlet),
		lockPath: filepath.Join(base, gauntlet+".lock"),
	}, nil
}

// world is the checkpointed root: everything the system under test owns.
func (h *home) world() string { return filepath.Join(h.root, "world") }

// harness is the reserved directory inside the world holding the state that
// describes the world at a boundary — the deposits, the world environment, the
// process registry. it rides every checkpoint, so those facts roll back with a
// restore exactly as the world does.
func (h *home) harness() string { return filepath.Join(h.world(), ".harness") }

// bin holds build artifacts. it lives beside the world, never inside it, so no
// checkpoint ever contains a binary and resume presses current code against
// restored state.
func (h *home) bin() string { return filepath.Join(h.root, "bin") }

// checkpointsDir holds the save-point chain.
func (h *home) checkpointsDir() string { return filepath.Join(h.root, "checkpoints") }

// logs holds supervised-process output, outside the checkpoint image: it describes
// the session, not the world at a boundary.
func (h *home) logs() string { return filepath.Join(h.root, "logs") }

// transcriptPath is the run's readable narrative.
func (h *home) transcriptPath() string { return filepath.Join(h.root, "transcript.md") }

// sessionPath holds the world generation and the corridor its checkpoints were
// made against.
func (h *home) sessionPath() string { return filepath.Join(h.root, "session.yaml") }

// runStatePath holds the run pointer and the fail-closed prune record.
func (h *home) runStatePath() string { return filepath.Join(h.root, "run.yaml") }

// ensure creates the tree's skeleton without disturbing anything already in it.
func (h *home) ensure() error {
	for _, d := range []string{h.root, h.world(), h.harness(), h.checkpointsDir(), h.logs(), h.bin()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("preparing %s: %w", d, err)
		}
	}
	return nil
}

// reset discards the world generation and starts a clean one, minting a session
// identity, recording the corridor its checkpoints will be made against, and
// publishing the fresh world as the genesis boundary.
//
// genesis belongs to reset rather than to the caller that sequences it. boundary
// zero is what makes the corridor's first challenge resolve like any other instead
// of being a special case, and a world generation that could exist without its own
// boundary zero is a coordinate system with a hole in it.
//
// bin survives: the bootstrap that produced the binary under test has already run
// by the time reset does, and the artifact it built is not world state.
func (h *home) reset(corridor []corridorEntry, runId string) (*session, error) {
	if err := h.ensure(); err != nil {
		return nil, err
	}
	for _, p := range []string{h.world(), h.checkpointsDir(), h.logs()} {
		if err := removeTree(p); err != nil {
			return nil, fmt.Errorf("resetting %s: %w", p, err)
		}
	}
	for _, p := range []string{h.transcriptPath(), h.sessionPath(), h.runStatePath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("resetting %s: %w", p, err)
		}
	}
	if err := h.ensure(); err != nil {
		return nil, err
	}
	s := newSession(corridor)
	if err := saveSession(h.sessionPath(), s); err != nil {
		return nil, err
	}
	if err := saveRunState(h.runStatePath(), &runState{}); err != nil {
		return nil, err
	}
	if _, err := newCheckpoints(h.checkpointsDir()).publish(0, genesisName, runId, h.world()); err != nil {
		return nil, fmt.Errorf("publishing the genesis boundary: %w", err)
	}
	return s, nil
}

// clean empties the tree explicitly, discarding the residue a failed run leaves
// behind. it removes only the tree's children — the lock is anchored outside, and
// unlinking it would defeat the guard that makes this operation safe.
func (h *home) clean() error {
	entries, err := os.ReadDir(h.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", h.root, err)
	}
	for _, e := range entries {
		if err := removeTree(filepath.Join(h.root, e.Name())); err != nil {
			return fmt.Errorf("cleaning %s: %w", h.root, err)
		}
	}
	return nil
}

// resumeMode is which save point a navigation is entitled to.
type resumeMode int

const (
	// resumeReplay is the rule behind naming a challenge to resume from: restore
	// the greatest boundary strictly before it and replay the challenges in
	// between. where an author traded snapshots away, resume pays in replay.
	resumeReplay resumeMode = iota
	// resumeExact is the rule behind running a single challenge: it needs its
	// immediate predecessor's boundary and nothing else will do, so a missing one
	// refuses plainly rather than silently reaching further back and replaying.
	resumeExact
)

// navigate is the whole of navigation, in one operation that cannot be entered
// halfway: refuse if an earlier navigation never finished, validate the corridor,
// resolve the boundary, record the prune intent, restore, clear the abandoned
// future, rebase the recorded corridor, and only then record the navigation
// complete. it returns the boundary the world now sits at.
//
// every step is here rather than at the call site because each one is only honest
// in this order. validation has to precede the restore or a divergent corridor
// gets restored before anyone notices. restoring and pruning are one thing because
// the instant the world moves back, every save point beyond that boundary
// describes a timeline that no longer exists, and a later navigation reaching one
// would report green against the wrong world. the rebase has to land before replay
// can publish anything, so the record and the world move together. and the intent
// is recorded first and cleared last, so a navigation that fails or is interrupted
// anywhere in between leaves the record set — there is no honest way to tell from
// the outside how much of the abandoned future survived.
//
// target is the one-based position of the challenge the invocation means to
// execute; current is the corridor it means to execute it in.
func (h *home) navigate(current []corridorEntry, target int, mode resumeMode) (checkpointRef, error) {
	if target < 1 || target > len(current) {
		return checkpointRef{}, fmt.Errorf("challenge position %d is outside a corridor of %d", target, len(current))
	}
	// the session comes first so a tree that holds no world generation says that
	// rather than complaining about a record it was never going to have.
	s, err := loadSession(h.sessionPath())
	if err != nil {
		return checkpointRef{}, err
	}
	if err := h.requireCompletePrune(); err != nil {
		return checkpointRef{}, err
	}
	if err := s.validatePrefix(current, target); err != nil {
		return checkpointRef{}, err
	}

	c := newCheckpoints(h.checkpointsDir())
	var ref checkpointRef
	var ok bool
	switch mode {
	case resumeExact:
		ref, ok, err = c.at(target - 1)
	default:
		ref, ok, err = c.greatestBelow(target)
	}
	if err != nil {
		return checkpointRef{}, err
	}
	if !ok {
		return checkpointRef{}, fmt.Errorf("%w before %q at position %d in session %s",
			errNoSavePoint, current[target-1].Name, target, s.Id)
	}

	rs, err := loadRunState(h.runStatePath())
	if err != nil {
		return checkpointRef{}, err
	}
	rs.Pruning, rs.PruningTo = true, ref.Boundary
	if err := saveRunState(h.runStatePath(), rs); err != nil {
		return checkpointRef{}, err
	}
	if err := c.restore(ref, h.world()); err != nil {
		return checkpointRef{}, err
	}
	if err := c.removeAbove(ref.Boundary); err != nil {
		return checkpointRef{}, err
	}
	s.rebase(current)
	if err := saveSession(h.sessionPath(), s); err != nil {
		return checkpointRef{}, err
	}
	rs.Pruning, rs.PruningTo = false, 0
	if err := saveRunState(h.runStatePath(), rs); err != nil {
		return checkpointRef{}, err
	}
	return ref, nil
}

// requireCompletePrune refuses navigation when an earlier one never finished.
func (h *home) requireCompletePrune() error {
	rs, err := loadRunState(h.runStatePath())
	if err != nil {
		return err
	}
	if rs.Pruning {
		return fmt.Errorf("%w: a navigation to boundary %d never finished clearing the abandoned future, which may still be selectable; clean the world to continue",
			errIncompletePrune, rs.PruningTo)
	}
	return nil
}
