package challenge

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/michaelquigley/df/dd"
)

// session is the world generation: the identity minted when the world was reset,
// and the corridor shape the residue beside it was made against. checkpoints are
// only meaningful against the narrative that produced them, so the narrative is
// recorded with them.
type session struct {
	Id        string
	CreatedAt time.Time
	Corridor  []corridorEntry
}

// corridorEntry records one challenge's position and checkpoint policy in the
// corridor a session's checkpoints were made against.
type corridorEntry struct {
	Name         string
	NoCheckpoint bool
}

// errDivergentCorridor is the harness fault a navigation earns when the current
// gauntlet no longer matches the corridor the session's checkpoints were made
// against.
var errDivergentCorridor = errors.New("divergent corridor")

// newId mints a short identity with a one-letter kind prefix, in the same register
// the practice's other brokers use. rand.Text cannot fail, so minting an identity
// has no error path to classify — the census stays a statement about the world
// under test rather than about the random source.
func newId(prefix string) string {
	return prefix + "_" + rand.Text()[:12]
}

// newSession mints a world generation for a freshly reset world, recording the
// corridor its checkpoints will be made against.
func newSession(corridor []corridorEntry) *session {
	return &session{
		Id:        newId("s"),
		CreatedAt: time.Now(),
		Corridor:  append([]corridorEntry(nil), corridor...),
	}
}

// loadSession reads the session record. a missing record means there is no world
// generation to resume; an unreadable or malformed one is harness-owned state the
// harness cannot trust, and says so rather than guessing.
func loadSession(path string) (*session, error) {
	s, err := dd.NewYAMLFile[session](path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("reading session state %s: %w", path, err)
	}
	return s, nil
}

// saveSession writes the session record.
func saveSession(path string, s *session) error {
	if err := dd.UnbindYAMLFile(s, path); err != nil {
		return fmt.Errorf("writing session state %s: %w", path, err)
	}
	return nil
}

// validatePrefix compares the current gauntlet's prefix through a target against
// the corridor this session's checkpoints were made against. a challenge inserted,
// removed, renamed, or reordered before the target means the old checkpoints
// describe a narrative that no longer exists, and restoring one would report green
// against the wrong world — so divergence in the prefix is a harness fault naming
// where it happened. a changed suffix is a different thing entirely: the resume is
// valid and the session rebases.
//
// through is the number of leading entries that must match — the target's
// one-based position, so the target itself participates.
//
// names govern divergence; a prefix challenge's checkpoint policy does not. the
// recorded policies describe which boundaries a corridor was expected to produce,
// and rebase keeps them current — but flipping one changes nothing about the
// world a surviving boundary holds. boundary N remains a truthful record of the
// world after challenge N whichever way the policy later moved, and restoring a
// prefix challenge's earlier output is what resume does by design. refusing here
// would discard a whole world generation over an authoring tweak that made no
// snapshot untrue.
func (s *session) validatePrefix(current []corridorEntry, through int) error {
	if through > len(current) {
		return fmt.Errorf("%w: session %s records %d challenges, the target sits at %d; clean the world to start a new generation",
			errDivergentCorridor, s.Id, len(s.Corridor), through)
	}
	if through > len(s.Corridor) {
		return fmt.Errorf("%w: session %s recorded %d challenges, the current gauntlet reaches %q at %d; clean the world to start a new generation",
			errDivergentCorridor, s.Id, len(s.Corridor), current[through-1].Name, through)
	}
	for i := 0; i < through; i++ {
		if s.Corridor[i].Name != current[i].Name {
			return fmt.Errorf("%w: session %s recorded %q at position %d, the current gauntlet has %q; clean the world to start a new generation",
				errDivergentCorridor, s.Id, s.Corridor[i].Name, i+1, current[i].Name)
		}
	}
	return nil
}

// rebase replaces the recorded corridor — names and checkpoint policies alike —
// with the one that actually exists. it runs after a valid navigation has cleared
// the abandoned future and before replay publishes anything, so the record and the
// world move together: the next navigation validates against the corridor the
// checkpoints were really made against.
func (s *session) rebase(current []corridorEntry) {
	s.Corridor = append([]corridorEntry(nil), current...)
}
