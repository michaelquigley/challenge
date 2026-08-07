//go:build linux

package challenge

import "golang.org/x/sys/unix"

// swapPaths exchanges two existing paths in one step.
//
// replacing a published boundary otherwise takes two renames, and between them the
// boundary exists at neither name — an interruption there leaves resume falling
// back to an earlier save point. that fails closed, but an exchange removes the
// window outright: before it, the canonical name is the old image; after it, the
// new one; never neither.
func swapPaths(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
