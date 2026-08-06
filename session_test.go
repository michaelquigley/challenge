package challenge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// corridorOf builds a recorded corridor shape from names, all checkpointing.
func corridorOf(names ...string) []corridorEntry {
	out := make([]corridorEntry, 0, len(names))
	for _, n := range names {
		out = append(out, corridorEntry{Name: n})
	}
	return out
}

func TestPrefixValidationRefusesADivergentPast(t *testing.T) {
	s := newSession(corridorOf("estate", "containers", "slices"))

	require.NoError(t, s.validatePrefix(corridorOf("estate", "containers", "slices"), 2))

	// renamed
	err := s.validatePrefix(corridorOf("estate", "cabinets", "slices"), 2)
	require.ErrorIs(t, err, errDivergentCorridor)
	assert.Contains(t, err.Error(), s.Id, "the refusal names the session it refused for")
	assert.Contains(t, err.Error(), "position 2", "and where the corridor diverged")

	// reordered
	assert.ErrorIs(t, s.validatePrefix(corridorOf("containers", "estate", "slices"), 2), errDivergentCorridor)

	// inserted before the target
	assert.ErrorIs(t, s.validatePrefix(corridorOf("estate", "quiesce", "containers", "slices"), 3), errDivergentCorridor)

	// removed, leaving the recorded corridor longer than the target's reach
	assert.ErrorIs(t, s.validatePrefix(corridorOf("estate"), 2), errDivergentCorridor)
}

func TestPrefixValidationAllowsAChangedFuture(t *testing.T) {
	s := newSession(corridorOf("estate", "containers", "slices"))
	branched := corridorOf("estate", "containers", "durability", "restore")

	// suites are meant to evolve: the past is intact, so the resume is valid and
	// the run branches.
	require.NoError(t, s.validatePrefix(branched, 2))

	// the target itself participates in the prefix, so a resume aimed into the
	// changed suffix still refuses until the record catches up.
	assert.ErrorIs(t, s.validatePrefix(branched, 3), errDivergentCorridor)

	// rebasing replaces the record with the corridor that actually exists, so the
	// next navigation validates against the world the checkpoints were made
	// against rather than one that was abandoned.
	s.rebase(branched)
	assert.NoError(t, s.validatePrefix(branched, 3))
	assert.NoError(t, s.validatePrefix(branched, 4))
}

func TestSessionAndRunStateRoundTrip(t *testing.T) {
	h := testHome(t)

	s := newSession([]corridorEntry{{Name: "estate"}, {Name: "containers", NoCheckpoint: true}})
	require.NoError(t, saveSession(h.sessionPath(), s))
	back, err := loadSession(h.sessionPath())
	require.NoError(t, err)
	assert.Equal(t, s.Id, back.Id)
	assert.Equal(t, s.Corridor, back.Corridor)

	// the run pointer is harness-owned state reset creates, and it carries the
	// fail-closed prune marker. treating its disappearance as "nothing was in
	// flight" would let the evidence the guarantee rests on be lost by exactly the
	// failure it guards against.
	_, err = loadRunState(h.runStatePath())
	assert.Error(t, err)

	rs := &runState{LastRunId: newId("r")}
	require.NoError(t, saveRunState(h.runStatePath(), rs))
	rs2, err := loadRunState(h.runStatePath())
	require.NoError(t, err)
	assert.Equal(t, rs.LastRunId, rs2.LastRunId)
}
