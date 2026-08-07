package challenge

import (
	"errors"
	"fmt"
)

// Challenge is the unit of authorship: a named function receiving the world,
// acting through the system's shipped surface, and recording findings.
//
// challenges are ordered, and each inherits the world its predecessor left behind.
// this is where the longitudinal narrative lives — pressure composes across
// challenges the way a real estate accumulates history.
type Challenge struct {
	// Name identifies the challenge in the corridor, in navigation, and in every
	// finding attributed to it.
	Name string
	// Doc carries the provenance behind the challenge: the regression it guards,
	// the ordering it depends on, the reason an odd check is there. that knowledge
	// is load-bearing, and it belongs beside the challenge rather than in
	// marginalia.
	Doc string
	// Fn is the challenge itself: ordinary Go, debuggable with ordinary tools.
	Fn func(*W)
	// NoCheckpoint trades this boundary's save point away. resume then pays in
	// replay rather than in a full re-run, and the trade is visible in the code at
	// the boundary that made it.
	NoCheckpoint bool
}

// Gauntlet is a project's ordered registry of challenges plus its world
// configuration. it is defined in code, and run whole or navigated by name.
type Gauntlet struct {
	// Name is the gauntlet's identity, and the directory its world lives in.
	Name string
	// WorldHome is the absolute path the world tree is anchored beneath.
	//
	// it is declared, never discovered. products record absolute paths inside their
	// own state, so a checkpoint restored anywhere but its original root is dead on
	// arrival — and standalone, make, and go test's temporary binary all have to
	// land on one world regardless of where they were invoked from.
	WorldHome string
	// Bootstrap is a consumer-owned hook the engine runs once the lock is held and
	// before the world is reset or restored.
	//
	// core prescribes nothing about what it does. producing the binary under test
	// is the usual job, but no project shape is assumed by the harness — the
	// consumer owns what, the engine owns when.
	Bootstrap func(*W) error
	// Challenges is the corridor, in order.
	Challenges []Challenge
}

// validate reports whether this is a gauntlet the engine can run at all.
//
// names are how navigation finds a challenge and how a session records the shape
// its checkpoints were made against, so a duplicate or missing one is not a
// cosmetic problem.
func (g Gauntlet) validate() error {
	if !gauntletName.MatchString(g.Name) {
		return fmt.Errorf("gauntlet name %q must be a plain path component of letters, digits, and .-_ starting with a letter or digit", g.Name)
	}
	if len(g.Challenges) == 0 {
		return errors.New("a gauntlet needs at least one challenge")
	}
	seen := map[string]bool{}
	for i, c := range g.Challenges {
		switch {
		case c.Name == "":
			return fmt.Errorf("challenge %d has no name", i+1)
		case seen[c.Name]:
			return fmt.Errorf("challenge %q appears twice; a name is how navigation finds a challenge", c.Name)
		case c.Fn == nil:
			return fmt.Errorf("challenge %q has no body", c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}

// corridor renders the gauntlet's shape for the session record: the ordered names
// and the checkpoint policy each one carries.
func (g Gauntlet) corridor() []corridorEntry {
	out := make([]corridorEntry, 0, len(g.Challenges))
	for _, c := range g.Challenges {
		out = append(out, corridorEntry{Name: c.Name, NoCheckpoint: c.NoCheckpoint})
	}
	return out
}

// indexOf finds a challenge's one-based position in the corridor.
func (g Gauntlet) indexOf(name string) (int, bool) {
	for i, c := range g.Challenges {
		if c.Name == name {
			return i + 1, true
		}
	}
	return 0, false
}

// names lists the corridor in order, for a refusal that wants to say what exists.
func (g Gauntlet) names() []string {
	out := make([]string, 0, len(g.Challenges))
	for _, c := range g.Challenges {
		out = append(out, c.Name)
	}
	return out
}
