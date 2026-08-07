package challenge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var (
	gauntletOnce sync.Once
	gauntletPath string
	gauntletErr  error
)

// gauntletBinary builds the toy consumer once for the whole test binary. it is a
// real package main calling challenge.Main, so the engine and the standalone face
// are exercised through the surface a consumer actually uses.
func gauntletBinary(t *testing.T) string {
	t.Helper()
	gauntletOnce.Do(func() {
		dir, err := os.MkdirTemp("", "challenge-toyg-")
		if err != nil {
			gauntletErr = err
			return
		}
		gauntletPath = filepath.Join(dir, "toygauntlet")
		out, err := exec.Command("go", "build", "-o", gauntletPath, "./internal/toygauntlet").CombinedOutput()
		if err != nil {
			gauntletErr = fmt.Errorf("building the toy gauntlet: %v: %s", err, out)
		}
	})
	require.NoError(t, gauntletErr)
	return gauntletPath
}

// suite is one toy consumer pointed at one world.
type suite struct {
	t    *testing.T
	bin  string
	home string
	env  []string
}

// newSuite anchors a toy consumer's world in a directory of its own.
func newSuite(t *testing.T) *suite {
	t.Helper()
	base := t.TempDir()
	t.Cleanup(func() { openTree(base) })
	return &suite{t: t, bin: gauntletBinary(t), home: base, env: []string{
		"TOYG_WORLD_HOME=" + base,
		"TOYG_TOY=" + toyBinary(t),
	}}
}

// with adds environment for the invocations that follow.
func (s *suite) with(kv ...string) *suite {
	s.env = append(s.env, kv...)
	return s
}

// invocation is one completed run of the toy consumer.
type invocation struct {
	exit   int
	stdout string
	stderr string
}

// out is everything the invocation rendered.
func (i invocation) out() string { return i.stdout + i.stderr }

// run invokes the toy consumer and waits for it.
func (s *suite) run(args ...string) invocation {
	s.t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = append(os.Environ(), s.env...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !asExitError(err, &exitErr) {
		s.t.Fatalf("invoking the toy gauntlet: %v", err)
	}
	return invocation{exit: cmd.ProcessState.ExitCode(), stdout: out.String(), stderr: errOut.String()}
}

// start invokes the toy consumer without waiting, for the cases that need to reach
// it while it runs.
func (s *suite) start(args ...string) *exec.Cmd {
	s.t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = append(os.Environ(), s.env...)
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	require.NoError(s.t, cmd.Start())
	s.t.Cleanup(func() { _ = killGroup(cmd.Process, unix.SIGKILL); _ = cmd.Wait() })
	return cmd
}

// world is the path of something inside the suite's world.
func (s *suite) world(rel ...string) string {
	return filepath.Join(append([]string{s.home, ".challenge", "toyg", "world"}, rel...)...)
}

// tree is the path of something in the suite's gauntlet tree.
func (s *suite) tree(rel ...string) string {
	return filepath.Join(append([]string{s.home, ".challenge", "toyg"}, rel...)...)
}

// asExitError reports whether an error is a command that ran and exited.
func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func TestACleanRunWalksTheWholeCorridor(t *testing.T) {
	s := newSuite(t)
	got := s.run()

	assert.Equal(t, 0, got.exit, got.out())
	for _, name := range []string{"estate", "containers", "slices", "durability", "restore"} {
		assert.Contains(t, got.out(), name)
	}
	assert.Contains(t, got.out(), "clean")
	assert.Contains(t, got.out(), "5 executed")

	// the fresh world is boundary zero, and every challenge closes one after it.
	refs, err := newCheckpoints(s.tree("checkpoints")).list()
	require.NoError(t, err)
	require.Len(t, refs, 6)
	assert.Equal(t, genesisName, refs[0].Name)
	assert.Equal(t, "restore", refs[5].Name)

	// the binary under test lives beside the world, so no checkpoint contains one.
	assert.FileExists(t, s.tree("bin", "toy"))
	for _, ref := range refs {
		assert.NoFileExists(t, filepath.Join(ref.image(), "bin", "toy"))
	}

	// and the narrative is there to read.
	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "## attempt")
	assert.Contains(t, string(body), "the digest was deposited by estate")
}

func TestTheThreeExitTiers(t *testing.T) {
	assert.Equal(t, 0, newSuite(t).run().exit)

	findings := newSuite(t).with("TOYG_MISBEHAVE=slices:assert").run()
	assert.Equal(t, 1, findings.exit, findings.out())
	assert.Contains(t, findings.out(), "assertion")

	// an assertion severs nothing, so the corridor continues through it.
	assert.Contains(t, findings.out(), "5 executed")

	fault := newSuite(t).with("TOYG_MISBEHAVE=slices:fault").run()
	assert.Equal(t, 2, fault.exit, fault.out())
	assert.Contains(t, fault.out(), "fault")
}

func TestATerminalFindingEndsTheCorridor(t *testing.T) {
	for how, class := range map[string]string{"break": "break", "crash": "crash"} {
		s := newSuite(t).with("TOYG_MISBEHAVE=containers:" + how)
		got := s.run()

		assert.Equal(t, 1, got.exit, got.out())
		assert.Contains(t, got.out(), class)
		assert.Contains(t, got.out(), "3 not run", "the corridor beneath a break is not marched")

		// and the window that broke publishes no save point, so resume cannot sail
		// past the break.
		refs, err := newCheckpoints(s.tree("checkpoints")).list()
		require.NoError(t, err)
		require.Len(t, refs, 2, how)
		assert.Equal(t, "estate", refs[1].Name)
	}
}

func TestTheVerdictIsTotalThroughTheEngine(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=slices:quiet-exit")
	got := s.run()

	// a command nobody asserted is still expected to have succeeded, and the run
	// says so rather than passing quietly.
	assert.Equal(t, 1, got.exit, got.out())
	assert.Contains(t, got.out(), "with nothing expecting it to")
}

func TestResumeRunsAgainstItsPredecessorsWorld(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	ledger, err := os.ReadFile(s.world("ledger"))
	require.NoError(t, err)
	require.Equal(t, "containers\n", string(ledger))

	// containers is not idempotent: it appends. its own checkpoint exists, so
	// resuming it could restore its aftermath and produce two lines. the coordinate
	// rule says otherwise — a challenge runs against the world its predecessor left.
	got := s.run("--from", "containers")
	assert.Equal(t, 0, got.exit, got.out())

	ledger, err = os.ReadFile(s.world("ledger"))
	require.NoError(t, err)
	assert.Equal(t, "containers\n", string(ledger), "resume restored estate's world, not containers'")

	// the prefix was restored rather than executed, and the model says so.
	assert.Contains(t, got.out(), "1 restored")
	assert.Contains(t, got.out(), "4 executed")
}

func TestResumeCarriesTheDepositForward(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	// slices reads a value estate deposited. a local would have been lost with the
	// process; the deposit rides the checkpoint, so a resumed run collects it.
	got := s.run("--from", "slices")
	assert.Equal(t, 0, got.exit, got.out())
	assert.NotContains(t, got.out(), "the deposit came back as")
}

func TestOnlyRunsOneChallengeAndRefusesWithoutItsPredecessor(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	got := s.run("--only", "slices")
	assert.Equal(t, 0, got.exit, got.out())
	assert.Contains(t, got.out(), "1 executed")

	// running one challenge alone means running it against exactly the world it
	// inherits, so a missing predecessor refuses plainly rather than reaching
	// further back.
	require.NoError(t, removeTree(s.tree("checkpoints", "02-containers")))
	refused := s.run("--only", "slices")
	assert.Equal(t, 2, refused.exit, refused.out())
	assert.Contains(t, refused.out(), "no save point")
}

func TestResumeAdvancesTheFrontier(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)
	before, err := newCheckpoints(s.tree("checkpoints")).list()
	require.NoError(t, err)
	require.Len(t, before, 6)

	// resuming branches history. the abandoned branch's save points must not remain
	// selectable, or a later navigation restores a timeline that no longer exists.
	got := s.run("--from", "containers", "--only", "")
	require.Equal(t, 0, got.exit, got.out())

	after, err := newCheckpoints(s.tree("checkpoints")).list()
	require.NoError(t, err)
	assert.Len(t, after, 6, "the corridor was walked again, so the chain refilled")

	// the run ids on the refilled boundaries are this invocation's, not the first's.
	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(body), "## attempt"), "attempts separate visibly")
}

func TestNavigationRefusesACorridorThatMoved(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	// a challenge reordered before the target means the old boundaries describe a
	// narrative that no longer exists.
	moved := newSuite(t)
	moved.home = s.home
	moved.env = append(s.env, "TOYG_REORDER=1")
	got := moved.run("--from", "slices")
	assert.Equal(t, 2, got.exit, got.out())
	assert.Contains(t, got.out(), "divergent corridor")
	assert.Contains(t, got.out(), "clean the world", "the refusal names the way forward")

	// and the way forward works.
	assert.Equal(t, 0, moved.run("--clean").exit)
	assert.Equal(t, 0, moved.run().exit)
}

func TestAChangedFutureBranchesAndRebases(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	// the past intact and the future changed: the resume is valid, and the session
	// rebases onto the corridor that now exists.
	branched := newSuite(t)
	branched.home = s.home
	branched.env = append(s.env, "TOYG_SUFFIX=1")
	got := branched.run("--from", "containers")
	require.Equal(t, 0, got.exit, got.out())

	// the rebase is what makes the branch resumable in turn.
	again := branched.run("--from", "recovery")
	assert.Equal(t, 0, again.exit, again.out())
}

func TestOptOutPaysInReplay(t *testing.T) {
	s := newSuite(t).with("TOYG_NO_CHECKPOINT=containers")
	require.Equal(t, 0, s.run().exit)

	refs, err := newCheckpoints(s.tree("checkpoints")).list()
	require.NoError(t, err)
	require.Len(t, refs, 5, "the boundary that opted out published nothing")

	// resuming past the gap restores further back and replays what lies between.
	got := s.run("--from", "slices")
	assert.Equal(t, 0, got.exit, got.out())
	assert.Contains(t, got.out(), "1 restored", "estate's boundary, since containers has none")
	assert.Contains(t, got.out(), "4 executed", "containers replayed, then the corridor from slices on")

	// the restore rolled the world back past containers, so the ledger existing at
	// all is the proof that replay re-ran it — and holding one line is the proof it
	// ran against estate's world rather than on top of its own earlier output.
	ledger, err := os.ReadFile(s.world("ledger"))
	require.NoError(t, err)
	assert.Equal(t, "containers\n", string(ledger))
}

func TestALockedWorldRefusesAndTouchesNothing(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	held, err := acquireLock(filepath.Join(s.home, ".challenge", "toyg.lock"))
	require.NoError(t, err)
	defer held.release()

	before, err := os.Stat(s.tree("bin", "toy"))
	require.NoError(t, err)

	// a concurrent invocation is a loud refusal, never a second world at a mangled
	// path — and it is refused before its bootstrap can rebuild the binary the run
	// in progress is using.
	got := s.run()
	assert.Equal(t, 2, got.exit, got.out())
	assert.Contains(t, got.out(), "locked by another run")

	after, err := os.Stat(s.tree("bin", "toy"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "the refused invocation never reached its bootstrap")
}

func TestTheWorldHomeIsDeclaredNotDiscovered(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	// one gauntlet driven from anywhere lands on one world. the second invocation
	// refusing on the lock is the proof it found the first.
	held, err := acquireLock(filepath.Join(s.home, ".challenge", "toyg.lock"))
	require.NoError(t, err)
	elsewhere := s.run()
	require.NoError(t, held.release())
	assert.Equal(t, 2, elsewhere.exit)
	assert.Contains(t, elsewhere.out(), "locked by another run")

	// and the override moves lock, artifacts, and world together.
	other := t.TempDir()
	t.Cleanup(func() { openTree(other) })
	got := s.run("--world-home", other)
	assert.Equal(t, 0, got.exit, got.out())
	assert.FileExists(t, filepath.Join(other, ".challenge", "toyg.lock"))
	assert.FileExists(t, filepath.Join(other, ".challenge", "toyg", "bin", "toy"))
	assert.FileExists(t, filepath.Join(other, ".challenge", "toyg", "world", "estate.yaml"))
}

func TestCleanDiscardsTheGeneration(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)
	require.FileExists(t, s.world("estate.yaml"))

	assert.Equal(t, 0, s.run("--clean").exit)
	entries, err := os.ReadDir(s.tree())
	require.NoError(t, err)
	assert.Empty(t, entries)

	// the lock is anchored beside the tree, so cleaning cannot unlink it.
	assert.FileExists(t, filepath.Join(s.home, ".challenge", "toyg.lock"))
	assert.Equal(t, 0, s.run().exit)
}

func TestListPrintsTheCorridor(t *testing.T) {
	s := newSuite(t).with("TOYG_NO_CHECKPOINT=containers")
	got := s.run("--list")

	assert.Equal(t, 0, got.exit)
	assert.Contains(t, got.stdout, "estate")
	assert.Contains(t, got.stdout, "containers")
	assert.Contains(t, got.stdout, "no checkpoint")
	assert.NoDirExists(t, s.tree("world"), "listing runs nothing")
}

func TestABootstrapThatFailsIsAHarnessFault(t *testing.T) {
	s := newSuite(t).with("TOYG_BOOTSTRAP_FAIL=1")
	got := s.run()

	assert.Equal(t, 2, got.exit, got.out())
	assert.Contains(t, got.out(), "bootstrap")
	assert.NoFileExists(t, s.world("estate.yaml"), "the corridor never started")
}

func TestAnInterruptedRunLeavesNothingAlive(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=durability:hang")
	cmd := s.start()

	// wait until the fixture is up and the corridor is inside the hanging challenge.
	deadline := time.Now().Add(30 * time.Second)
	for !fileHasLine(s.tree("logs", "server.log"), "serving on port") {
		require.True(t, time.Now().Before(deadline), "the fixture never started")
		time.Sleep(50 * time.Millisecond)
	}

	// a keyboard interrupt reaches the foreground process group. harness children
	// are in groups of their own, so it arrives only at the harness — which records
	// the interruption first and then stops what it started.
	require.NoError(t, killGroup(cmd.Process, unix.SIGINT))
	waitFor(t, cmd)
	assert.Equal(t, 2, cmd.ProcessState.ExitCode(), "an interrupted run is an invalid run")

	// nothing it started outlived it.
	assert.False(t, anyToyAlive(t, s.tree()), "the unwind stopped every supervised process")
}

func TestAnInterruptIsNotAProductCrash(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=durability:hang", "TOYG_TRANSCRIPT=1")
	cmd := s.start()

	deadline := time.Now().Add(30 * time.Second)
	for !fileHasLine(s.tree("logs", "server.log"), "serving on port") {
		require.True(t, time.Now().Before(deadline), "the fixture never started")
		time.Sleep(50 * time.Millisecond)
	}
	require.NoError(t, cmd.Process.Signal(unix.SIGTERM))
	waitFor(t, cmd)

	// the crash tier is only worth having if it cannot be triggered by the
	// harness's own hand.
	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(body), "**crash**")
	assert.Contains(t, string(body), "interrupted")
}

// waitFor waits for an invocation to finish. a nonzero wire status is an outcome,
// not a failure to run.
func waitFor(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	var exitErr *exec.ExitError
	if err := cmd.Wait(); err != nil && !asExitError(err, &exitErr) {
		t.Fatalf("waiting for the toy gauntlet: %v", err)
	}
}

// fileHasLine reports whether a file contains a substring yet.
func fileHasLine(path, want string) bool {
	body, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(body), want)
}

// anyToyAlive reports whether any supervised toy is still running against a world.
func anyToyAlive(t *testing.T, tree string) bool {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", filepath.Join(tree, "bin", "toy")).Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

func TestARestartFailureKeepsTheBoundaryAndAttributesForward(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=durability:restart-fail")
	got := s.run()

	assert.Equal(t, 1, got.exit, got.out())

	// the failure struck after the boundary was published, and that snapshot is a
	// truthful closed world after a completed challenge — so it stays.
	refs, err := newCheckpoints(s.tree("checkpoints")).list()
	require.NoError(t, err)
	require.Len(t, refs, 5)
	assert.Equal(t, "durability", refs[4].Name)

	// and the finding belongs to the challenge that never got its fixture, whose
	// body never ran. resuming from the retained boundary retries the restart,
	// which is exactly the place a debugger wants to return to.
	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "### durability — executed")
	assert.Contains(t, string(body), "### restore — not-run")
	assert.Contains(t, string(body), "checkpoint: `04-durability`")
	assert.Contains(t, string(body), "**crash**", "the finding that explains the not-run is in the narrative")
}

func TestAttemptsSeparateAndAccountForThemselves(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=slices:assert")
	first := s.run()
	require.Equal(t, 1, first.exit, first.out())

	// the same world, resumed past the failure by a run told to misbehave nowhere.
	clean := newSuite(t)
	clean.home = s.home
	clean.env = []string{"TOYG_WORLD_HOME=" + s.home, "TOYG_TOY=" + toyBinary(t)}
	second := clean.run("--from", "durability")
	require.Equal(t, 0, second.exit, second.out())

	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	text := string(body)

	// two attempts, visibly apart, each accounting for its own work. a verdict is
	// per-invocation: the resumed run exits clean beside the first attempt's
	// finding rather than inheriting it.
	assert.Equal(t, 2, strings.Count(text, "## attempt "))
	assert.Contains(t, text, "verdict: findings (exit 1)")
	assert.Contains(t, text, "verdict: clean (exit 0)")
	assert.Contains(t, text, "told to fail an assertion")

	// and the second attempt says which challenges it did not execute.
	after := text[strings.LastIndex(text, "## attempt "):]
	assert.Contains(t, after, "### slices — restored")
	assert.Contains(t, after, "### durability — executed")
	assert.NotContains(t, after, "told to fail an assertion")
}

func TestAPartialTranscriptSurvivesAnAbruptEnding(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=slices:fault")
	got := s.run()
	require.Equal(t, 2, got.exit, got.out())

	// the transcript is the document you read when the guardian suite goes red, so
	// the steps that completed before the fault have to be in it.
	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	text := string(body)
	assert.Contains(t, text, "### estate — executed")
	assert.Contains(t, text, "$ toy state estate.yaml est-1 personal")
	assert.Contains(t, text, "**fault**")
	assert.Contains(t, text, "### restore — not-run")
}

func TestATranscriptThatCannotBeWrittenEndsTheRun(t *testing.T) {
	// this is the document you read when the guardian suite goes red, so a harness
	// that cannot produce it has failed at something it promised — and a harness
	// fault is terminal rather than a note beside a run that carried on.
	sealed := t.TempDir()
	require.NoError(t, os.Chmod(sealed, 0o555))
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })

	s := newSuite(t)
	got := s.run("--transcript", filepath.Join(sealed, "transcript.md"))
	assert.Equal(t, 2, got.exit, got.out())
	assert.Contains(t, got.out(), "writing the transcript")
	assert.Contains(t, got.out(), "not run", "it stopped rather than walking on")

	// an unreadable prior transcript is its own fault: the harness cannot say what
	// earlier attempts recorded, and carrying on would silently discard them.
	blocked := filepath.Join(t.TempDir(), "transcript.md")
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	unreadable := newSuite(t).run("--transcript", blocked)
	assert.Equal(t, 2, unreadable.exit, unreadable.out())
	assert.Contains(t, unreadable.out(), "reading the transcript")
}

func TestASuitePanicIsAHarnessFault(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=durability:suite-panic")
	got := s.run()

	// a suite that panics is a broken suite. letting that escape would take the
	// harness down with the run unaccounted for, the lock released, and whatever it
	// started still running.
	assert.Equal(t, 2, got.exit, got.out())
	assert.Contains(t, got.out(), "the suite panicked")
	assert.Contains(t, got.out(), "1 not run", "the corridor stopped there")

	// the world was left closed, and the narrative explains why.
	assert.False(t, anyToyAlive(t, s.tree()), "the unwind still ran")
	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "the suite panicked")
}

func TestAFreshRunDiscardsTheOldGenerationsNarrative(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=slices:assert")
	require.Equal(t, 1, s.run().exit)

	before, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	require.Contains(t, string(before), "told to fail an assertion")

	// a fresh run resets the world, and the attempts against the generation it
	// discarded go with it. a transcript carrying attempts against a world that no
	// longer exists is not a projection of anything.
	clean := newSuite(t)
	clean.home = s.home
	clean.env = []string{"TOYG_WORLD_HOME=" + s.home, "TOYG_TOY=" + toyBinary(t)}
	require.Equal(t, 0, clean.run().exit)

	after, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(after), "## attempt "))
	assert.NotContains(t, string(after), "told to fail an assertion")
}

func TestRenderersOnlySeeSettledRecords(t *testing.T) {
	s := newSuite(t).with("TOYG_MISBEHAVE=durability:restart-fail")
	got := s.run()
	require.Equal(t, 1, got.exit, got.out())

	// the restart failure lands on the challenge after the boundary, and cleanup
	// settles it. a record rendered before that would have said "ok" beside a
	// finding the model was about to record — so nothing is rendered until the
	// engine has moved past it and everything that could add to it has run.
	body, err := os.ReadFile(s.tree("transcript.md"))
	require.NoError(t, err)
	text := string(body)
	for _, name := range []string{"estate", "containers", "slices", "durability", "restore"} {
		assert.Contains(t, text, "### "+name+" — ")
	}
	assert.Contains(t, text, "### restore — not-run")
	assert.Contains(t, text, "**crash**")

	// and the console said it once: the finding, beside the challenge it belongs to,
	// not repeated by a record reported before and after it settled.
	assert.Equal(t, 1, strings.Count(got.out(), "crash: "))
}

func TestASavePointsIdentityTravelsWithIt(t *testing.T) {
	s := newSuite(t)
	require.Equal(t, 0, s.run().exit)

	// the manifest is the authority and the directory name is an index. relabelling
	// a save point does not change what it is, and filing one boundary's world under
	// another's coordinate is refused rather than restored.
	require.NoError(t, os.Rename(s.tree("checkpoints", "01-estate"), s.tree("checkpoints", "01-cabinets")))
	misfiled := s.run("--from", "containers")
	assert.Equal(t, 2, misfiled.exit, misfiled.out())
	assert.Contains(t, misfiled.out(), "is filed as boundary 1 after \"cabinets\"")
	assert.Contains(t, misfiled.out(), "published as boundary 1 after \"estate\"")

	// the world it would have restored was left where it was.
	ledger, err := os.ReadFile(s.world("ledger"))
	require.NoError(t, err)
	assert.Equal(t, "containers\n", string(ledger))
}
