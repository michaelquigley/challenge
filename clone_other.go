//go:build !linux

package challenge

import "os"

// cloneFile has no reflink path off linux, so every copy is the honest full one.
// the posture is linux-first; other hosts stay functional through the fallback.
func cloneFile(dst, src *os.File) error {
	return errNoClone
}
