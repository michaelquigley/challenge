package challenge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiteralsSubstituteWholeArgvTokens(t *testing.T) {
	w, cur, _ := testW(t)

	// there is no shell, so a substituted value is never split and never needs
	// quoting: a token containing spaces rides a placeholder whole.
	res := w.Run("toy args {} {}", "an album name", "second")
	assert.Equal(t, []string{"arg an album name", "arg second"}, res.Msgs)
	assert.Empty(t, cur.Findings)

	// non-string arguments are rendered rather than refused, because a port or a
	// job id reads better at the call site than a conversion does.
	res = w.Run("toy args {}", 9001)
	assert.Equal(t, []string{"arg 9001"}, res.Msgs)
}

func TestALiteralTheHarnessCannotIssueIsAFault(t *testing.T) {
	w, _, _ := testW(t)

	// a command the harness could not even issue says so at the harness's tier,
	// and no zero-valued result is manufactured to carry it onward.
	for name, issue := range map[string]func(){
		"empty literal":        func() { w.Run("   ") },
		"too few arguments":    func() { w.Run("toy args {} {}", "only-one") },
		"too many arguments":   func() { w.Run("toy args {}", "one", "two") },
		"embedded placeholder": func() { w.Run("toy args --name={}", "x") },
		"missing binary":       func() { w.Run("reef estate init personal") },
	} {
		unwound, class := capture(issue)
		assert.True(t, unwound, name)
		assert.Equal(t, ClassFault, class, name)
	}
}

func TestTheVerdictIsTotal(t *testing.T) {
	w, cur, _ := testW(t)

	// a command nobody asked about is still expected to have succeeded. this is
	// the loud abort the bash could never give its silenced setup lines.
	w.Run("toy fail something went wrong")
	assert.Empty(t, cur.Findings, "the implicit expectation resolves when the challenge ends")

	w.resolvePending()
	require.Len(t, cur.Findings, 1)
	assert.Equal(t, ClassAssertion, cur.Findings[0].Class)
	assert.Contains(t, cur.Findings[0].Message, "exited 1 with nothing expecting it to")

	// naming an expectation displaces the implicit one.
	cur.Findings = nil
	w.Run("toy fail something went wrong").ExpectExit(1)
	w.resolvePending()
	assert.Empty(t, cur.Findings)
}

func TestWordingIsContract(t *testing.T) {
	w, cur, _ := testW(t)

	w.Run("toy fail already initialized").
		ExpectExit(1).
		ExpectMsg("already initialized").
		ExpectMsgOnce("already initialized").
		ExpectNoMsg("stack trace")
	assert.Empty(t, cur.Findings)

	// an operational failure renders exactly once. a message that arrives twice
	// travelled two paths to the terminal, and counting the raw streams is what
	// catches the duplicate that parsing would fold away.
	w.Run("toy twice already initialized").ExpectExit(1).ExpectMsgOnce("already initialized")
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "rendered")

	cur.Findings = nil
	w.Run("toy emit nothing to see").ExpectMsg("something else")
	require.Len(t, cur.Findings, 1)
	assert.Equal(t, ClassAssertion, cur.Findings[0].Class)
}

func TestRawStreamsAreTheirOwnChannel(t *testing.T) {
	w, cur, _ := testW(t)

	// tables are not dl messages. they carry variable cell padding, so assertions
	// against them target single cells and header words, never phrases spanning
	// columns.
	w.Run("toy table").ExpectOut("satisfied").ExpectOut("status").ExpectNoOut("goroutine")
	assert.Empty(t, cur.Findings)

	// an error path renders to stderr, and the two streams stay separate rather
	// than being merged before anyone looks at them.
	res := w.Run("toy fail the drive is gone").ExpectExit(1).ExpectErr("the drive is gone")
	assert.Empty(t, res.Stdout)
	assert.Empty(t, cur.Findings)

	w.Run("toy emit fine").ExpectErr("anything at all")
	require.Len(t, cur.Findings, 1)
}

func TestCaptureDemandsExactlyOneMatch(t *testing.T) {
	w, cur, _ := testW(t)

	// the digest appears literally inside the raw line that carries it, so no
	// parsed-message indirection is needed.
	digest := w.Run("toy digest").Capture(`snapshot ([0-9a-f]{12})`)
	assert.Equal(t, "9f2c1ab77e40", digest)
	assert.Empty(t, cur.Findings)

	// the whole match when the pattern has no group.
	assert.Equal(t, "snapshot 9f2c1ab77e40", w.Run("toy digest").Capture(`snapshot [0-9a-f]{12}`))

	// zero matches: the output did not say what the suite claims it says. that is
	// the product's surface failing, and it is terminal so the break surfaces
	// where it happened rather than ripening into a missing deposit later.
	unwound, class := capture(func() {
		w.Run("toy emit nothing here").Capture(`snapshot ([0-9a-f]{12})`)
	})
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)

	// ambiguity is the same failure: the suite cannot say which one it meant.
	unwound, class = capture(func() {
		w.Run("toy digests").Capture(`snapshot ([0-9a-f]{12})`)
	})
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)

	// a broken expression is the harness's own fault, not the product's.
	unwound, class = capture(func() { w.Run("toy digest").Capture(`snapshot ([0-9a-f`) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
}

func TestAFailedCaptureNeverRipensIntoAFault(t *testing.T) {
	w, cur, _ := testW(t)

	// the reference suite's shape: capture in one challenge, deposit it, collect it
	// in the next. when the capture fails, the run has to end at the product's tier
	// rather than arriving two challenges later as a missing deposit — the same
	// break wearing the wrong class.
	unwound, class := capture(func() {
		w.Put("old-snap", w.Run("toy emit no digest here").Capture(`snapshot ([0-9a-f]{12})`))
	})
	require.True(t, unwound)
	assert.Equal(t, ClassBreak, class)
	assert.Equal(t, 0, w.run.Count(ClassFault))
	assert.Equal(t, 1, len(cur.Findings))
}

func TestStdinIsTheRealThing(t *testing.T) {
	w, cur, _ := testW(t)

	w.Cmd("toy prompt").Stdin("n\n").Run().
		ExpectExit(1).
		ExpectMsg("declined")
	assert.Empty(t, cur.Findings)

	w.Cmd("toy prompt").Stdin("y\n").Run().ExpectMsg("proceeding")
	assert.Empty(t, cur.Findings)
}

func TestEnvironmentArrivesFromBothWorldAndInvocation(t *testing.T) {
	w, cur, _ := testW(t)

	w.Setenv("TOY_WORKSPACE", "/world/ws")
	w.Run("toy env TOY_WORKSPACE").ExpectMsg("TOY_WORKSPACE=/world/ws")

	// an invocation override is the foreign-workspace probe: one command pointed
	// somewhere else, leaving the world's own fact alone.
	w.Cmd("toy env TOY_WORKSPACE").Env("TOY_WORKSPACE", "/elsewhere").Run().
		ExpectMsg("TOY_WORKSPACE=/elsewhere")
	w.Run("toy env TOY_WORKSPACE").ExpectMsg("TOY_WORKSPACE=/world/ws")
	assert.Empty(t, cur.Findings)
}

func TestTheTransportDefaultStaysUnderTest(t *testing.T) {
	w, cur, _ := testW(t)

	// dl selects its JSON transport from the fact that a harness subprocess is
	// piped. forcing an override would pass this suite while quietly retiring
	// dl's own default-selection from test.
	for _, kv := range w.childEnv(nil) {
		assert.False(t, strings.HasPrefix(kv, "DL_USE_JSON="), "the harness adds no transport override")
	}

	res := w.Run("toy emit parsed through an ordinary pipe")
	require.Equal(t, []string{"parsed through an ordinary pipe"}, res.Msgs)
	assert.Contains(t, res.Stdout, `"msg"`, "the child really did render its JSON transport")
	assert.Empty(t, cur.Findings)
}

func TestCommandsRunFromTheWorld(t *testing.T) {
	w, cur, _ := testW(t)

	w.Run("toy state estate.yaml est-1 personal").ExpectMsg("wrote state")
	w.Exists("estate.yaml")

	var mirror estateMirror
	w.ReadYAML("estate.yaml", &mirror)
	assert.Equal(t, "est-1", mirror.Id)
	assert.Equal(t, "personal", mirror.Label)

	// somewhere else on request, for the probes that need it.
	elsewhere := w.Dir("recovery")
	w.Cmd("toy state estate.yaml est-2 recovered").Dir(elsewhere).Run()
	w.Exists("recovery/estate.yaml")
	assert.Empty(t, cur.Findings)
}

func TestCrashesAreDetectedWithoutBeingAskedFor(t *testing.T) {
	w, cur, _ := testW(t)

	// the textual arm: a marker in either stream.
	unwound, class := capture(func() { w.Run("toy panic") })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "crashed")

	// the evidential arm: a death by signal, with no marker and no output at all.
	w2, cur2, _ := testW(t)
	unwound, class = capture(func() { w2.Run("toy kill") })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur2.Findings, 1)
	assert.Contains(t, cur2.Findings[0].Message, "signal")

	// one crash is one finding, however much evidence it left.
	assert.Equal(t, 1, w.run.Count(ClassCrash))
}

func TestProvenanceIsAboutThisCommandNotTheRun(t *testing.T) {
	w, cur, _ := testW(t)

	// the crash tier is only worth having if it cannot be triggered by the
	// harness's own hand — and equally, it must not be erased by an interruption
	// that did not cause the death. a run being cancelled around a command says
	// nothing about a command that killed itself.
	w.cancelled.Store(true)
	unwound, class := capture(func() { w.Run("toy kill") })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "signal")
}

func TestBinariesResolveAgainstTheWorldAdjacentBin(t *testing.T) {
	w, _, h := testW(t)

	// the suite exercises the binary the bootstrap just produced, not whatever
	// happens to be on PATH.
	installed := filepath.Join(h.bin(), "toy")
	assert.Equal(t, installed, w.resolveBinary("toy"))

	info, err := os.Stat(installed)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111)
}

func TestACrashIsNeverInventedFromTwoStreams(t *testing.T) {
	w, cur, _ := testW(t)

	// a fragment ending one stream and a fragment beginning the other were never
	// adjacent anywhere, and the highest-order finding in the census is not one to
	// synthesize out of two halves.
	res := w.Run("toy split-marker")
	assert.Equal(t, "pan", res.Stdout)
	assert.Equal(t, "ic: not really\n", res.Stderr)
	assert.Empty(t, cur.Findings)
	assert.Equal(t, 0, w.run.Count(ClassCrash))
}

func TestACommandThatLeavesAWriterBehindIsAFault(t *testing.T) {
	w, cur, _ := testW(t)

	// a product that backgrounds a child and exits leaves a writer nothing is
	// supervising: it is not a declared fixture, so quiescence never reaches it, and
	// a boundary snapshot taken while it works would copy a world still being
	// changed. a suite that wants a long-lived process declares one.
	unwound, class := capture(func() { w.Run("toy daemon") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "left processes running")
}

func TestACrashOutranksTheStragglerItLeft(t *testing.T) {
	w, cur, _ := testW(t)

	// two things are true about this invocation and only one of them is about the
	// product. reporting the harness's housekeeping complaint first would hide the
	// crash evidence behind it.
	unwound, class := capture(func() { w.Run("toy daemon --panic") })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "crashed")
	assert.Equal(t, 0, w.run.Count(ClassFault))
}
