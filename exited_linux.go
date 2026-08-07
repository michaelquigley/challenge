//go:build linux

package challenge

import (
	"errors"

	"golang.org/x/sys/unix"
)

// alreadyExited reports whether a process has terminated, as of right now, without
// waiting on anything.
//
// this is the primitive provenance needs and nothing else provides. the harness's
// own wait tells it a process is gone only once the wait returns, which can be well
// after the death — so a fixture that fell over a moment before its boundary could
// be signalled, appear to have been stopped by that signal, and lose the crash it
// earned. a pidfd becomes readable the instant its process dies and keeps answering
// for a zombie, so polling one settles the question at the moment it is asked.
//
// a pid that no longer exists at all answers the same way: gone.
func alreadyExited(pid int) (bool, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return true, nil
		}
		return false, err
	}
	defer unix.Close(fd)

	ready, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 0)
	if err != nil {
		return false, err
	}
	return ready > 0, nil
}
