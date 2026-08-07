//go:build !linux

package challenge

// alreadyExited has no exact answer off linux, where there is no pidfd to poll.
//
// reporting "not yet" rather than refusing keeps other unix hosts working: the
// timing heuristic behind it still catches an exit the harness observed before it
// asked for one, and the manner-of-death rules still catch the rest. what is lost
// is only the narrowest window — a fixture that dies in the instant between the
// probe and the signal — and the posture is linux-first, where that window is
// closed exactly.
func alreadyExited(pid int) (bool, error) {
	return false, nil
}
