package challenge

import (
	"errors"
	"fmt"
	"os"

	"github.com/michaelquigley/df/dd"
)

// runState is the mutable pointer beside the session: the last invocation's
// identity, and the fail-closed record of a navigation in flight.
type runState struct {
	LastRunId string
	// Pruning records that a navigation started clearing the future it abandoned
	// and never recorded finishing. finding it set on arrival means that future
	// may still be selectable, so navigation refuses until the world is cleaned.
	// the zero value is the safe one — but only within a record that exists.
	Pruning bool
	// PruningTo is the boundary the in-flight navigation was collapsing to.
	PruningTo int
}

// errIncompletePrune is the harness fault a navigation earns when an earlier one
// never finished clearing the future it abandoned. an incomplete prune can leave
// that future selectable, so navigation fails closed rather than guessing.
var errIncompletePrune = errors.New("incomplete checkpoint prune")

// loadRunState reads the run pointer.
//
// absence is a fault rather than a fresh start. this file is harness-owned state
// that reset creates and that carries the fail-closed prune marker; treating its
// disappearance as "nothing was in flight" would let exactly the evidence the
// guarantee rests on be lost by the thing it guards against.
func loadRunState(path string) (*runState, error) {
	rs, err := dd.NewYAMLFile[runState](path)
	if err != nil {
		return nil, fmt.Errorf("reading run state %s: %w", path, err)
	}
	return rs, nil
}

// saveRunState writes the run pointer and flushes it, because the fail-closed
// record is only worth having if it survives the failure it guards against.
func saveRunState(path string, rs *runState) error {
	data, err := dd.UnbindYAML(rs)
	if err != nil {
		return fmt.Errorf("marshaling run state: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing run state %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing run state %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flushing run state %s: %w", path, err)
	}
	return nil
}
