//go:build linux

package challenge

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile asks the filesystem to share the source's extents with the
// destination — reflink-first copying, in-process, with no external cp
// dependency. filesystems without copy-on-write refuse, and the caller falls back
// to an honest full copy.
//
// none of this practice's operating environments run a reflink-capable
// filesystem, so the full copy is the working cost everywhere; the attempt is kept
// because it is free, not because there is a plan to collect on it.
func cloneFile(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}
