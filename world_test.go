package challenge

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lastFinding returns the finding a challenge most recently recorded.
func lastFinding(t *testing.T, cur *ChallengeRun) *Finding {
	t.Helper()
	require.NotEmpty(t, cur.Findings)
	return cur.Findings[len(cur.Findings)-1]
}

func TestDirCreatesAndPathDoesNot(t *testing.T) {
	w, _, h := testW(t)

	d := w.Dir("restore", "target")
	assert.Equal(t, filepath.Join(h.world(), "restore", "target"), d)
	info, err := os.Stat(d)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	p := w.Path("nas", "album")
	assert.Equal(t, filepath.Join(h.world(), "nas", "album"), p)
	_, err = os.Stat(p)
	assert.Error(t, err, "Path names a location, it does not make one")

	// a path handed out by Dir can be handed back.
	assert.Equal(t, d, w.Path(d))
}

func TestPathRefusesToLeaveTheWorld(t *testing.T) {
	w, _, h := testW(t)

	// outside the world lie the harness's own session state and the checkpoint
	// images. a suite path that climbs out is a broken harness input, and letting
	// one through would corrupt the world the verdict is supposed to be about.
	for _, escape := range []string{
		"../session.yaml",
		"../../..",
		"nas/../../checkpoints",
		filepath.Join(h.root, "run.yaml"),
		"/etc/passwd",
	} {
		unwound, class := capture(func() { w.Path(escape) })
		assert.True(t, unwound, escape)
		assert.Equal(t, ClassFault, class, escape)
	}

	// traversal that stays inside is ordinary path arithmetic, not an escape.
	assert.Equal(t, filepath.Join(h.world(), "nas"), w.Path("nas/album/.."))
	assert.Equal(t, h.world(), w.Path())
}

func TestWritesRefuseToFollowALinkOutOfTheWorld(t *testing.T) {
	w, _, h := testW(t)

	// the world under test is full of links the product put there, and a lexically
	// innocent path through one lands physically outside — where the harness's own
	// session state and the checkpoint images live.
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "untouched"), []byte("original"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(h.world(), "media")))

	unwound, class := capture(func() { w.WriteFile("media/untouched", []byte("overwritten")) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)

	unwound, class = capture(func() { w.Dir("media", "fresh") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)

	body, err := os.ReadFile(filepath.Join(outside, "untouched"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(body), "the refused write touched nothing")
	assert.NoDirExists(t, filepath.Join(outside, "fresh"))

	// observing a link is still a legitimate thing for a challenge to do; only
	// writing through one is refused. and the ordinary case still works.
	refusals := len(w.cur.Findings)
	w.Exists("media")
	w.Absent("media/never-written")
	w.WriteFile("ws/config.yaml", []byte("bind: 127.0.0.1:9000\n"))
	w.Exists("ws/config.yaml")
	assert.Len(t, w.cur.Findings, refusals, "containment is about writes, not observations")
}

func TestDepositsRideTheCheckpointAndRollBackWithIt(t *testing.T) {
	w, _, h := testW(t)
	c := newCheckpoints(h.checkpointsDir())

	w.Put("old-snap", "9f2c1ab77e40")
	w.Setenv("REEF_WORKSPACE", "/world/ws")
	ref, err := c.publish(1, "durability", "s_test", "r_test", h.world())
	require.NoError(t, err)

	// a deposit and an environment fact made after the save point are facts about
	// a future the restore abandons.
	w.Put("new-snap", "deadbeefcafe")
	w.Setenv("REEF_DEBUG", "1")
	assert.Equal(t, "deadbeefcafe", w.Get("new-snap"))

	require.NoError(t, c.restore(ref, "s_test", h.world()))
	require.NoError(t, w.reload())

	assert.Equal(t, "9f2c1ab77e40", w.Get("old-snap"), "the deposit rides the checkpoint")
	assert.Equal(t, []string{"REEF_WORKSPACE=/world/ws"}, w.envPairs())

	unwound, class := capture(func() { w.Get("new-snap") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class, "a deposit that never happened is harness-owned state, not a product finding")
}

func TestFreePortIsUsable(t *testing.T) {
	w, _, _ := testW(t)
	port := w.FreePort()
	assert.NotZero(t, port)

	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "the harness hands out a port the vocabulary can actually bind")
	require.NoError(t, l.Close())
}

func TestExistsAndAbsentDistinguishAbsenceFromIgnorance(t *testing.T) {
	w, cur, h := testW(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "present"), []byte("x"), 0o644))

	w.Exists("present")
	w.Absent("missing")
	assert.Empty(t, cur.Findings, "the true statements record nothing")

	w.Exists("missing")
	assert.Equal(t, ClassAssertion, lastFinding(t, cur).Class)

	w.Absent("present")
	assert.Equal(t, ClassAssertion, lastFinding(t, cur).Class)

	// absence is only ever ENOENT. a path the harness cannot stat at all is the
	// harness's problem, and reading it as "not there" is how a suite passes on a
	// world it never saw.
	sealed := filepath.Join(h.world(), "sealed")
	require.NoError(t, os.MkdirAll(sealed, 0o755))
	require.NoError(t, os.Chmod(sealed, 0o000))
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })

	unwound, class := capture(func() { w.Absent("sealed/inside") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)

	unwound, class = capture(func() { w.Exists("sealed/inside") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
}

func TestSameBytesComparesLargeObjects(t *testing.T) {
	w, cur, h := testW(t)
	body := strings.Repeat("payload", 1<<18)
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "source"), []byte(body), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "restored"), []byte(body), 0o644))

	w.SameBytes("source", "restored")
	assert.Empty(t, cur.Findings)

	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "tampered"), []byte(body[:len(body)-1]+"X"), 0o644))
	w.SameBytes("source", "tampered")
	require.Len(t, cur.Findings, 1)
	assert.Equal(t, ClassAssertion, cur.Findings[0].Class)

	w.SameBytes("source", "never-written")
	require.Len(t, cur.Findings, 2)
	assert.Contains(t, cur.Findings[1].Message, "never-written")
}

func TestReadFileBreaksRatherThanReturningNothing(t *testing.T) {
	w, cur, h := testW(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "job.log"), []byte("staged 3/3"), 0o644))
	assert.Equal(t, "staged 3/3", string(w.ReadFile("job.log")))

	// the value travels onward, so bytes that are not there sever dependent flow.
	unwound, class := capture(func() { w.ReadFile("job.log.missing") })
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)
	assert.Equal(t, ClassBreak, lastFinding(t, cur).Class)
}

// estateMirror is the shape a vocabulary would define for itself: narrow, and
// declaring what it depends on, which is what turns drift into signal.
type estateMirror struct {
	Id    string `dd:"+required"`
	Label string
}

func TestTypedReadsCarryTheDecodeTiers(t *testing.T) {
	w, _, h := testW(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "estate.yaml"),
		[]byte("id: est-1\nlabel: personal\nunrelated: 7\n"), 0o644))

	var m estateMirror
	w.ReadYAML("estate.yaml", &m)
	assert.Equal(t, "est-1", m.Id)
	assert.Equal(t, "personal", m.Label)

	// a mutated field name on the shipped surface is the suite catching a format
	// change, and it lands as a break rather than a zero value travelling onward.
	require.NoError(t, os.WriteFile(filepath.Join(h.world(), "estate.yaml"),
		[]byte("identifier: est-1\nlabel: personal\n"), 0o644))
	var drifted estateMirror
	unwound, class := capture(func() { w.ReadYAML("estate.yaml", &drifted) })
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)

	// an unusable destination is the harness's own fault, not a statement about the
	// product — and it has to be caught before the bytes are read, or the binder's
	// complaint about the destination reads as a complaint about the product.
	var typedNil *estateMirror
	var notAStruct int
	for name, target := range map[string]any{
		"nil":                     nil,
		"typed nil":               typedNil,
		"non-pointer":             estateMirror{},
		"pointer to a non-struct": &notAStruct,
	} {
		unwound, class = capture(func() { w.ReadYAML("estate.yaml", target) })
		assert.True(t, unwound, name)
		assert.Equal(t, ClassFault, class, name)
	}
}

func TestVerdictIsComputedFromTheModelAlone(t *testing.T) {
	run := &Run{Gauntlet: "toy"}
	assert.Equal(t, 0, run.Verdict())

	cur := &ChallengeRun{Name: "estate"}
	run.Challenges = append(run.Challenges, cur)
	cur.Findings = append(cur.Findings, &Finding{Class: ClassAssertion, Message: "wording"})
	assert.Equal(t, 1, run.Verdict())

	cur.Findings = append(cur.Findings, &Finding{Class: ClassCrash, Message: "panic"})
	assert.Equal(t, 1, run.Verdict(), "a crash is a finding, and findings exit 1")

	run.Findings = append(run.Findings, &Finding{Class: ClassFault, Message: "lock"})
	assert.Equal(t, 2, run.Verdict(), "a harness fault invalidates the run")

	// an interrupted run is an invalid run whether or not it managed to record
	// anything on the way out.
	assert.Equal(t, 2, (&Run{Interrupted: true}).Verdict())

	assert.Equal(t, 1, run.Count(ClassAssertion))
	assert.Equal(t, 1, run.Count(ClassCrash))
	assert.Equal(t, 1, run.Count(ClassFault))
	assert.Equal(t, 0, run.Count(ClassBreak))
}

func TestFindingClassNamesItself(t *testing.T) {
	// every renderer names the class, so the class names itself once.
	assert.Equal(t, "assertion", ClassAssertion.String())
	assert.Equal(t, "break", ClassBreak.String())
	assert.Equal(t, "crash", ClassCrash.String())
	assert.Equal(t, "fault", ClassFault.String())

	assert.False(t, ClassAssertion.Terminal(), "the corridor continues through a wording mismatch")
	for _, c := range []FindingClass{ClassBreak, ClassCrash, ClassFault} {
		assert.True(t, c.Terminal(), "%s ends the invocation", c)
	}
}
