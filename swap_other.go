//go:build !linux

package challenge

import "errors"

// errNoSwap reports that this host offers no atomic path exchange, so replacing a
// published boundary falls back to two renames.
var errNoSwap = errors.New("no atomic path exchange")

// swapPaths has no single-step form off linux.
func swapPaths(a, b string) error {
	return errNoSwap
}
