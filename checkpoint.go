package challenge

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michaelquigley/df/dl"
	"golang.org/x/sys/unix"
)

// genesisName is the name boundary zero carries: the fresh world, checkpointed at
// reset so the corridor's first challenge resolves like any other rather than
// being a special case.
const genesisName = "genesis"

// tmpPrefix marks a checkpoint still being built. a checkpoint becomes selectable
// only by rename, so a failed or interrupted copy is never a save point.
const tmpPrefix = ".building-"

// errUnsupportedNode is the harness fault a snapshot earns when the world holds a
// node type the copy contract cannot carry faithfully. a snapshot that cannot be
// faithful refuses to exist rather than skipping silently.
var errUnsupportedNode = errors.New("unsupported node type")

// errNoClone reports that this host offers no reflink path, so the honest full
// copy carries the file instead.
var errNoClone = errors.New("no reflink support")

// errNoSavePoint is what a navigation earns when the boundary it needs was never
// published — a challenge that opted out, under the exact-predecessor rule, or a
// corridor that never ran that far.
var errNoSavePoint = errors.New("no save point")

// copyTree copies src to dst honestly.
//
// the contract covers more than bytes: regular-file contents, file and directory
// mode bits, modification times, empty directories, and symlinks preserved as
// links rather than dereferenced. any other node type — socket, device, fifo — is
// a harness fault, never a silent skip.
//
// metadata is applied in a second, deepest-first pass. writing a child re-stamps
// its parent's mtime, and a directory restored to a read-only mode before its
// children are written cannot receive them, so modes and times land only once the
// subtree beneath them is complete.
//
// mode fidelity includes the special bits. setuid, setgid, and sticky change what
// the filesystem does, not merely who may read — a setgid media directory that
// comes back without its bit is a different world.
func copyTree(src, dst string) error {
	type meta struct {
		path string
		mode fs.FileMode
		mod  time.Time
	}
	var dirs []meta

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			// created permissively; the real mode and time land in the second pass.
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, meta{path: target, mode: preservedMode(info.Mode()), mod: info.ModTime()})
			return nil

		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			// a symlink's own timestamp is observable through lstat, so it is part
			// of the world a scanner sees.
			return lchtimes(target, info.ModTime())

		case info.Mode().IsRegular():
			return copyFile(p, target, info)

		default:
			return fmt.Errorf("%w %s at %s", errUnsupportedNode, info.Mode().Type(), p)
		}
	})
	if err != nil {
		return err
	}

	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chtimes(d.path, d.mod, d.mod); err != nil {
			return err
		}
	}
	return nil
}

// copyFile carries one regular file, attempting a reflink first and falling back
// to an honest full copy. mode bits and the modification time follow the bytes.
func copyFile(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := cloneFile(out, in); err != nil {
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			out.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, preservedMode(info.Mode())); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// preservedMode is the mode a faithful copy carries: the permission bits plus the
// special bits that change observable filesystem behavior rather than merely who
// may read.
func preservedMode(m fs.FileMode) fs.FileMode {
	return m.Perm() | (m & (fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky))
}

// lchtimes sets a path's modification time without following it, so a symlink
// carries its own timestamp rather than stamping the file it points at.
func lchtimes(path string, mod time.Time) error {
	ts := unix.NsecToTimespec(mod.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}

// removeTree deletes a tree the world may have made read-only. a product that
// flips a directory to 0555 — reef's dying-drive drill does exactly that — leaves
// a tree os.RemoveAll cannot unlink, so the modes are opened as the walk descends.
func removeTree(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// ignore the result: an un-chmodable directory surfaces as the removal
			// error below, which carries the better message.
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// checkpointRef names one published save point: the boundary it closes and the
// challenge it followed. boundary N is the world after challenge N; boundary zero
// is genesis, the fresh world at reset.
type checkpointRef struct {
	Boundary int
	Name     string
	Dir      string
}

// checkpoints owns one world's save-point chain.
type checkpoints struct {
	dir string
}

// newCheckpoints opens the chain rooted at dir.
func newCheckpoints(dir string) *checkpoints {
	return &checkpoints{dir: dir}
}

// list returns every published save point in boundary order. directories still
// being built are not save points and never appear.
func (c *checkpoints) list() ([]checkpointRef, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading checkpoints %s: %w", c.dir, err)
	}
	var refs []checkpointRef
	seen := map[int]string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			continue
		}
		// a save point replaced by something that is not a directory is corrupted
		// harness-owned state, not an optional checkpoint that happens to be
		// missing. skipping it would let navigation quietly fall back to an earlier
		// boundary instead of saying the world cannot be trusted.
		if !e.IsDir() {
			return nil, fmt.Errorf("checkpoint entry %s in %s is not a directory", e.Name(), c.dir)
		}
		boundary, name, ok := parseCheckpointName(e.Name())
		if !ok {
			return nil, fmt.Errorf("unrecognized checkpoint directory %s in %s", e.Name(), c.dir)
		}
		// a boundary names exactly one closed world. two directories claiming the
		// same one would let the exact-predecessor rule and the strictly-before
		// rule restore different worlds from the same coordinate.
		if other, dup := seen[boundary]; dup {
			return nil, fmt.Errorf("boundary %d is claimed by both %s and %s in %s", boundary, other, e.Name(), c.dir)
		}
		seen[boundary] = e.Name()
		refs = append(refs, checkpointRef{Boundary: boundary, Name: name, Dir: filepath.Join(c.dir, e.Name())})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Boundary < refs[j].Boundary })
	return refs, nil
}

// at returns the save point closing a given boundary.
func (c *checkpoints) at(boundary int) (checkpointRef, bool, error) {
	refs, err := c.list()
	if err != nil {
		return checkpointRef{}, false, err
	}
	for _, r := range refs {
		if r.Boundary == boundary {
			return r, true, nil
		}
	}
	return checkpointRef{}, false, nil
}

// greatestBelow returns the greatest save point strictly before a challenge's
// one-based position.
//
// this is the coordinate rule the whole save-point model turns on: checkpoints are
// post-challenge boundaries, so reproducing challenge N means restoring the
// greatest boundary before it and executing N against its predecessor's world —
// never against its own aftermath.
func (c *checkpoints) greatestBelow(index int) (checkpointRef, bool, error) {
	refs, err := c.list()
	if err != nil {
		return checkpointRef{}, false, err
	}
	var best checkpointRef
	found := false
	for _, r := range refs {
		if r.Boundary < index {
			best, found = r, true
		}
	}
	return best, found, nil
}

// publish snapshots a closed world as the save point for a boundary.
//
// the copy builds under a temporary sibling and renames into its canonical name
// only when it completes, so a failed or interrupted copy is never selectable by
// the resume resolver.
func (c *checkpoints) publish(boundary int, name, runId, world string) (checkpointRef, error) {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return checkpointRef{}, fmt.Errorf("preparing checkpoints %s: %w", c.dir, err)
	}
	canonical := filepath.Join(c.dir, checkpointDirName(boundary, name))
	building := filepath.Join(c.dir, tmpPrefix+checkpointDirName(boundary, name)+"-"+runId)

	if err := removeTree(building); err != nil {
		return checkpointRef{}, fmt.Errorf("clearing a stale build directory %s: %w", building, err)
	}
	if err := os.MkdirAll(building, 0o755); err != nil {
		return checkpointRef{}, fmt.Errorf("preparing %s: %w", building, err)
	}
	if err := copyTree(world, building); err != nil {
		// the failed copy is discarded rather than left behind: an unfaithful
		// snapshot must not survive as anything a resolver could reach.
		_ = removeTree(building)
		return checkpointRef{}, fmt.Errorf("snapshotting the world into %s: %w", building, err)
	}
	// a boundary being republished is retired by rename rather than deleted in
	// place. deleting first would mean a failure partway through could leave a
	// half-erased image standing at the canonical name, where the resolver would
	// find it and restore a world that was never saved. renaming keeps every
	// intermediate state honest: the old complete image, or the new one.
	replacing := false
	if _, err := os.Lstat(canonical); err == nil {
		replacing = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = removeTree(building)
		return checkpointRef{}, fmt.Errorf("inspecting %s: %w", canonical, err)
	}

	switch {
	case !replacing:
		if err := os.Rename(building, canonical); err != nil {
			_ = removeTree(building)
			return checkpointRef{}, fmt.Errorf("publishing %s: %w", canonical, err)
		}

	case swapPaths(building, canonical) == nil:
		// the exchange put the new image at the canonical name and the old one
		// where the build was. the boundary was never absent for an instant.
		if err := removeTree(building); err != nil {
			return checkpointRef{}, fmt.Errorf("discarding the replaced image %s: %w", building, err)
		}

	default:
		// no single-step exchange here, so the old image is stood aside and the new
		// one moved in. the window between the two renames leaves the boundary
		// briefly at neither name, which fails closed: resume falls back to an
		// earlier save point rather than reaching a partial one.
		retired := filepath.Join(c.dir, tmpPrefix+"retired-"+checkpointDirName(boundary, name)+"-"+runId)
		if err := os.Rename(canonical, retired); err != nil {
			_ = removeTree(building)
			return checkpointRef{}, fmt.Errorf("retiring %s: %w", canonical, err)
		}
		if err := os.Rename(building, canonical); err != nil {
			_ = removeTree(building)
			// the boundary is put back rather than left absent.
			_ = os.Rename(retired, canonical)
			return checkpointRef{}, fmt.Errorf("publishing %s: %w", canonical, err)
		}
		if err := removeTree(retired); err != nil {
			return checkpointRef{}, fmt.Errorf("discarding the retired image %s: %w", retired, err)
		}
	}
	dl.Debugf("published checkpoint %s", canonical)
	return checkpointRef{Boundary: boundary, Name: name, Dir: canonical}, nil
}

// restore replaces the world with a save point's image. the copy is the same
// honest one publication used, so a restored world is indistinguishable from the
// world that was saved.
func (c *checkpoints) restore(ref checkpointRef, world string) error {
	if err := removeTree(world); err != nil {
		return fmt.Errorf("clearing the world %s: %w", world, err)
	}
	if err := os.MkdirAll(world, 0o755); err != nil {
		return fmt.Errorf("preparing the world %s: %w", world, err)
	}
	if err := copyTree(ref.Dir, world); err != nil {
		return fmt.Errorf("restoring %s into %s: %w", ref.Dir, world, err)
	}
	dl.Debugf("restored checkpoint %s", ref.Dir)
	return nil
}

// removeAbove deletes every save point past a boundary. resuming branches history,
// and the abandoned branch's save points must not remain selectable — a later
// navigation would otherwise restore a timeline that no longer exists and report
// green against the wrong world.
func (c *checkpoints) removeAbove(boundary int) error {
	refs, err := c.list()
	if err != nil {
		return err
	}
	for _, r := range refs {
		if r.Boundary > boundary {
			if err := removeTree(r.Dir); err != nil {
				return fmt.Errorf("removing the abandoned checkpoint %s: %w", r.Dir, err)
			}
		}
	}
	return nil
}

// clearBuilding removes directories left behind by an interrupted publication.
func (c *checkpoints) clearBuilding() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading checkpoints %s: %w", c.dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := removeTree(filepath.Join(c.dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkpointDirName renders a boundary's canonical directory name.
func checkpointDirName(boundary int, name string) string {
	return fmt.Sprintf("%02d-%s", boundary, safeName(name))
}

// parseCheckpointName reads a boundary and challenge name back out of a directory
// name.
func parseCheckpointName(dir string) (int, string, bool) {
	cut := strings.Index(dir, "-")
	if cut <= 0 {
		return 0, "", false
	}
	boundary, err := strconv.Atoi(dir[:cut])
	if err != nil {
		return 0, "", false
	}
	return boundary, dir[cut+1:], true
}

// safeName reduces a challenge name to something a directory name can carry
// without surprises.
func safeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "challenge"
	}
	return b.String()
}
