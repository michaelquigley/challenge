package challenge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestFixturesAreRegisteredAndBounced(t *testing.T) {
	w, cur, h := testW(t)
	f, _ := serveFixture(t, "server")

	w.Start(f)
	require.Empty(t, cur.Findings)
	w.On("server").Get("/api/v1/config").ExpectStatus(200).ExpectBody(`"name":"toy"`)

	// the declaration persists inside the checkpoint image, so a resumed run knows
	// what to bring back up.
	specs, err := readProcessRegistry(filepath.Join(h.harness(), processName))
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, "server", specs[0].Name)
	assert.Equal(t, "/api/v1/config", specs[0].ReadyURL)

	// the bounce is the ordinary boundary: down for the snapshot, back up before
	// the next challenge, and the world answers again on the other side.
	w.Quiesce()
	assert.Empty(t, w.instances)
	w.Restart()
	w.On("server").Get("/api/v1/config").ExpectStatus(200)
	assert.Empty(t, cur.Findings)
}

func TestTheFixtureRegistryRollsBackWithTheWorld(t *testing.T) {
	w, cur, h := testW(t)
	f, _ := serveFixture(t, "server")
	c := newCheckpoints(h.checkpointsDir())

	ref, err := c.publish(1, "before", "r_test", h.world())
	require.NoError(t, err)

	w.Start(f)
	w.Quiesce()
	require.Len(t, w.specs, 1)

	// a fixture a later challenge registered is a fact about a future the restore
	// abandons, so a resumed run never starts it.
	require.NoError(t, c.restore(ref, h.world()))
	require.NoError(t, w.reload())
	assert.Empty(t, w.specs, "the registry rides the checkpoint image")
	assert.Empty(t, cur.Findings)
}

func TestStartFailuresCarryTheirOwnTiers(t *testing.T) {
	// a spawn failure or an invalid probe declaration is the harness or the suite
	// being broken.
	w, _, _ := testW(t)
	for name, start := range map[string]func(){
		"no readiness declared": func() { w.Start(Fixture{Name: "server", Literal: "toy serve --port 1"}) },
		"probe without a base": func() {
			w.Start(Fixture{Name: "server", Literal: "toy serve --port 1", ReadyURL: "/api/v1/config"})
		},
		"unnamed": func() { w.Start(Fixture{Literal: "toy serve --port 1", ReadyFile: "x"}) },
		"missing binary": func() {
			w.Start(Fixture{Name: "server", Literal: "nonesuch serve", ReadyFile: "x"})
		},
	} {
		unwound, class := capture(start)
		assert.True(t, unwound, name)
		assert.Equal(t, ClassFault, class, name)
	}
}

func TestAFixtureThatDiesWhileStartingIsACrash(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--never-ready", "--die-after 200ms")
	f.ReadyTimeout = 10 * time.Second

	// the process came apart on its own. that is the product's highest-order
	// failure, and it ends the invocation rather than marching a corridor against a
	// fixture that does not exist.
	unwound, class := capture(func() { w.Start(f) })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "died while starting")
}

func TestAFixtureThatNeverBecomesReadyIsABreak(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--never-ready")
	f.ReadyTimeout = 1500 * time.Millisecond

	// live, answering, and never usable. the product failed to become operational
	// through its shipped surface, which is a different statement from crashing —
	// and the same outcome, because the corridor beneath it cannot proceed.
	unwound, class := capture(func() { w.Start(f) })
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "never became ready")
}

func TestAFixtureFoundDeadAtItsBoundaryIsACrashExactlyOnce(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--die-after 300ms")

	w.Start(f)
	require.Empty(t, cur.Findings)
	time.Sleep(700 * time.Millisecond)

	// a supervised process found dead before its requested quiesce is a crash event
	// in its own right, marker or no marker.
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "already dead at its boundary")
	assert.Contains(t, cur.Findings[0].Detail, "exited 7")

	// one crash is one finding however many surfaces observe it. the unwind's own
	// best-effort quiesce must not report it a second time.
	w.shutdown()
	assert.Equal(t, 1, w.run.Count(ClassCrash))
}

func TestAPanickingFixtureIsFoundInItsOwnWindow(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--panic-after 300ms")

	w.Start(f)
	time.Sleep(700 * time.Millisecond)

	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Detail, "panic:")
}

func TestAnOldPanicIsNeverReattributed(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server")

	// the same log file is appended across every instance of a fixture. each start
	// opens its window at the file's current size, so a boundary reads only what
	// this instance wrote — and a panic from an earlier life is never re-discovered
	// by a later boundary or a resumed run.
	w.Start(f)
	logPath := filepath.Join(w.home.logs(), "server.log")
	require.NoError(t, appendTo(logPath, "\npanic: from a previous life\n"))
	w.instances["server"].offset = fileSize(t, logPath)

	w.Quiesce()
	w.Restart()
	w.Quiesce()
	assert.Empty(t, cur.Findings, "an old panic belongs to the instance that wrote it")
	assert.Equal(t, 0, w.run.Count(ClassCrash))
}

func TestAFixtureThatWillNotCloseIsAHarnessFault(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--slow-stop 30s")
	f.StopTimeout = 500 * time.Millisecond

	w.Start(f)

	// quiescence assumes clean closes: a process holding a lock and a write-ahead
	// log has to release them. a world snapshotted around a process that had to be
	// killed is not a closed one, and the run says so loudly rather than
	// snapshotting it anyway.
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "not a closed one")
	assert.Equal(t, 0, w.run.Count(ClassCrash), "a process the harness killed is not a product crash")
}

func TestSupervisedProcessesRunInTheirOwnGroup(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// harness-spawned processes are isolated from the runner's foreground group, so
	// a keyboard interrupt reaches only the harness and children die by the
	// harness's recorded hand rather than by a stray tty signal that would
	// masquerade as a product crash.
	inst := w.instances["server"]
	pgid, err := unixGetpgid(inst.cmd.Process.Pid)
	require.NoError(t, err)
	assert.Equal(t, inst.cmd.Process.Pid, pgid, "the fixture leads its own group")

	runner, err := unixGetpgid(os.Getpid())
	require.NoError(t, err)
	assert.NotEqual(t, runner, pgid, "and it is not the runner's")
}

// appendTo adds to a file the way a supervised process would.
func appendTo(path, body string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(body)
	return err
}

// fileSize reports a file's current length.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

func TestRedeclaringALiveFixtureIsAFault(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// replacing the registration would strand the running process outside both the
	// registry and the instance table, where quiescence cannot reach it — and a
	// live writer nobody can stop is exactly what a boundary snapshot must never be
	// taken around.
	again, _ := serveFixture(t, "server")
	unwound, class := capture(func() { w.Start(again) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)

	// the original is still supervised, and still reachable.
	require.Len(t, w.specs, 1)
	assert.False(t, w.instances["server"].exited())
}

func TestAPanicOnTheWayDownIsStillACrash(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--panic-on-stop")
	f.StopTimeout = 5 * time.Second
	w.Start(f)

	// provenance excludes the harness-initiated death itself, not the product's own
	// account of coming apart while it happened. a fixture that panics in its
	// shutdown path would otherwise pass for a clean close.
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Detail, "panic:")
}

func TestACleanCloseLeavesNoFinding(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// the ordinary boundary: asked to stop, stopped, nothing to report.
	w.Quiesce()
	assert.Empty(t, cur.Findings)
	assert.Equal(t, 0, w.run.Count(ClassCrash))
}

func TestACorruptRegistryLeavesThroughTheHarnessTier(t *testing.T) {
	h := testHome(t)
	path := filepath.Join(h.harness(), processName)

	// binding says the document was shaped like a registry, not that it describes
	// fixtures anything can start. an entry that would come apart at the restart
	// has to be caught here rather than as an uncontrolled panic that would read
	// like the harness itself crashing.
	for name, body := range map[string]string{
		"no command":   "fixtures:\n  - name: server\n    ready_url: /x\n    base_url: http://127.0.0.1:1\n",
		"no name":      "fixtures:\n  - argv: [toy]\n    ready_url: /x\n    base_url: http://127.0.0.1:1\n",
		"no readiness": "fixtures:\n  - name: server\n    argv: [toy]\n",
		"no timeouts":  "fixtures:\n  - name: server\n    argv: [toy]\n    ready_file: x\n",
		"duplicated":   "fixtures:\n  - name: server\n    argv: [toy]\n    ready_file: x\n    ready_timeout: 1s\n    stop_timeout: 1s\n  - name: server\n    argv: [toy]\n    ready_file: x\n    ready_timeout: 1s\n    stop_timeout: 1s\n",
	} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		_, err := readProcessRegistry(path)
		assert.Error(t, err, name)
	}

	// and a whole one still loads.
	require.NoError(t, os.WriteFile(path,
		[]byte("fixtures:\n  - name: server\n    argv: [toy, serve]\n    ready_file: x\n    ready_timeout: 1s\n    stop_timeout: 1s\n"), 0o644))
	specs, err := readProcessRegistry(path)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, []string{"toy", "serve"}, specs[0].Argv)
}

func TestADeathRacingItsRequestIsStillOneFinding(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// killing the group directly is the race the classification has to survive: the
	// refusal arrives before the wait goroutine has caught up with the death that
	// caused it. reporting a break now and a crash at cleanup would double the
	// counts on the harness's highest-order signal.
	require.NoError(t, killGroup(w.instances["server"].cmd.Process, unix.SIGKILL))

	unwound, class := capture(func() { w.On("server").Get("/api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)

	w.shutdown()
	assert.Equal(t, 1, w.run.Count(ClassCrash))
	assert.Equal(t, 0, w.run.Count(ClassBreak))
	require.Len(t, cur.Findings, 1)
}

func TestQuiescenceMeansTheWholeGroup(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--orphan")
	f.StopTimeout = 2 * time.Second
	w.Start(f)

	// the leader exiting is not the same as the fixture being gone. a process it
	// started can outlive it and keep writing into the world, and a snapshot taken
	// around that writer is exactly the torn image the bounce exists to prevent.
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "not a closed one")
}

func TestADeadFixtureCannotBeQuietlyReplaced(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--die-after 300ms")
	w.Start(f)
	time.Sleep(700 * time.Millisecond)

	// declaring over a fixture that died on its own would bury both the death and
	// the window that explains it.
	again, _ := serveFixture(t, "server")
	unwound, class := capture(func() { w.Start(again) })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "found dead when it was declared again")
}

func TestAnInterruptedStartupMakesNoProductClaim(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--never-ready")
	f.ReadyTimeout = 30 * time.Second

	// fixtures deliberately do not die by an automatic context kill, so the
	// readiness wait is the synchronous path that has to notice an interruption.
	// waiting it out would report "never became ready" — a product claim for a
	// startup the harness itself cut short.
	go func() {
		time.Sleep(300 * time.Millisecond)
		w.cancelled.Store(true)
	}()
	started := time.Now()
	unwound, class := capture(func() { w.Start(f) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Less(t, time.Since(started), 10*time.Second, "it leaves as soon as it knows")
	assert.Empty(t, cur.Findings, "one interruption earns one finding, where it was received")
	assert.Equal(t, 2, w.run.Verdict())
}

func TestAProbeTheHarnessCannotIssueIsAFault(t *testing.T) {
	w, _, _ := testW(t)

	// a probe that can never succeed would spend the whole readiness timeout
	// failing and then blame the product for never becoming ready.
	for name, f := range map[string]Fixture{
		"unusable base":    {Name: "server", Literal: "toy serve --port 1", BaseURL: "not a url", ReadyURL: "/api/v1/config"},
		"schemeless base":  {Name: "server", Literal: "toy serve --port 1", BaseURL: "127.0.0.1:9000", ReadyURL: "/api/v1/config"},
		"negative timeout": {Name: "server", Literal: "toy serve --port 1", ReadyFile: "x", ReadyTimeout: -time.Second},
	} {
		unwound, class := capture(func() { w.Start(f) })
		assert.True(t, unwound, name)
		assert.Equal(t, ClassFault, class, name)
	}
}

func TestProvenanceSurvivesADeathAtTheBoundary(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// killing it and quiescing with no pause is the race provenance has to survive:
	// the process is gone but the wait has not caught up. treating that as a death
	// the harness asked for would lose the crash and let a checkpoint publish from
	// that window.
	require.NoError(t, killGroup(w.instances["server"].cmd.Process, unix.SIGKILL))
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)

	// whichever way the observations landed — the wait caught up first, or the
	// manner of the death gave it away afterward — it is one crash, named.
	assert.Contains(t, cur.Findings[0].Message, `"server"`)
	assert.Contains(t, cur.Findings[0].Message, "at its boundary")
	assert.Contains(t, cur.Findings[0].Detail, "signal killed")
}

func TestCancellationProvenanceComesFromTheContextToo(t *testing.T) {
	h := testHome(t)
	installToy(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{Gauntlet: "toy", RunId: newId("r")}
	cur := &ChallengeRun{Name: "probe", Status: StatusExecuted}
	run.Challenges = append(run.Challenges, cur)
	w, err := newW(ctx, h, run, cur)
	require.NoError(t, err)
	t.Cleanup(func() { w.shutdown() })

	// whoever cancels the context is the harness. reading only the recorded flag
	// would let one interruption arrive as a product crash on the command channel
	// and a product break on the wire.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	unwound, class := capture(func() { w.Run("toy sleep 10s") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Equal(t, 0, run.Count(ClassCrash), "the harness's own kill is not a product crash")
	assert.Empty(t, cur.Findings)
	assert.True(t, run.Interrupted)
	assert.Equal(t, 2, run.Verdict(), "an interrupted run is an invalid run, and the model says so itself")
}

func TestARegistryProbeTheHarnessCannotIssueIsAFault(t *testing.T) {
	h := testHome(t)
	path := filepath.Join(h.harness(), processName)

	// a malformed probe arriving from the registry is no less the harness's own
	// state than one arriving from a declaration, and restarting into it would
	// spend the readiness timeout failing and then blame the product.
	require.NoError(t, os.WriteFile(path,
		[]byte("fixtures:\n  - name: server\n    argv: [toy]\n    base_url: not a url\n    ready_url: /api/v1/config\n    ready_timeout: 1s\n    stop_timeout: 1s\n"), 0o644))
	_, err := readProcessRegistry(path)
	assert.Error(t, err)
}

func TestAFixtureOwnsItsEnvironment(t *testing.T) {
	w, _, _ := testW(t)
	env := map[string]string{"TOY_MODE": "first"}
	f, _ := serveFixture(t, "server")
	f.Env = env
	w.Start(f)

	// the registry is world state that rides a checkpoint. holding the caller's map
	// would let a later mutation move the in-memory topology while the checkpointed
	// image stayed where it was.
	env["TOY_MODE"] = "second"
	assert.Equal(t, "first", w.specs[0].Env["TOY_MODE"])
}

func TestABoundaryRefusesToSnapshotALiveWorld(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// a supervised process still holding open state can be mid-write while the copy
	// walks the tree, and the resulting image would be a torn store nothing
	// downstream could tell from a real one. the refusal is a property of the
	// operation, not a rule the caller has to remember.
	unwound, class := capture(func() { w.publishBoundary(1, "durability") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "not been through a boundary")

	refs, err := newCheckpoints(w.home.checkpointsDir()).list()
	require.NoError(t, err)
	assert.Empty(t, refs, "nothing was published")

	// closed world, honest snapshot.
	cur.Findings = nil
	w.Quiesce()
	ref, err := w.publishBoundary(1, "durability")
	require.NoError(t, err)
	assert.Equal(t, 1, ref.Boundary)
	assert.Empty(t, cur.Findings)
}

func TestAProbeSchemeTheClientCannotSpeakIsAFault(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	f.BaseURL = "ftp://127.0.0.1:9000"

	// it parses, it has a host, and the harness still cannot issue it — so it would
	// spend the readiness timeout failing and then blame the product.
	unwound, class := capture(func() { w.Start(f) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
}

func TestADeadLeaderStillClearsItsGroup(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--orphan", "--die-after 300ms")
	f.StopTimeout = 3 * time.Second
	w.Start(f)
	time.Sleep(700 * time.Millisecond)

	// the leader is gone and something it started is not. anything still in the
	// group is still a writer, so the group is cleared before the death is reported
	// — and nothing can publish a boundary until it is.
	pid := w.instances["server"].cmd.Process.Pid
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.True(t, groupGone(pid), "the descendants went with it")
}

func TestPublicationSeesDescendantsToo(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--orphan")
	w.Start(f)

	// leader exiting, descendant alive: the instance is not gone, and a boundary
	// taken here would copy a world something is still writing to.
	inst := w.instances["server"]
	require.NoError(t, inst.cmd.Process.Signal(unix.SIGKILL))
	<-inst.done

	unwound, class := capture(func() { w.publishBoundary(1, "durability") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Contains(t, cur.Findings[0].Message, "not been through a boundary")
	assert.True(t, clearGroup(inst.cmd.Process.Pid, 3*time.Second))
}

func TestAQuietlyDeadFixtureStillBlocksABoundary(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--die-after 200ms")
	w.Start(f)
	time.Sleep(700 * time.Millisecond)

	// gone, reaped, and never accounted for. quiescence is where an unsolicited
	// death is observed and classified, so publishing around a fixture that skipped
	// it would make a checkpoint selectable from a crash window nobody has looked
	// at.
	assert.True(t, w.instances["server"].exited())
	unwound, class := capture(func() { w.publishBoundary(1, "durability") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Contains(t, cur.Findings[0].Message, "not been through a boundary")

	refs, err := newCheckpoints(w.home.checkpointsDir()).list()
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestARegistryThatIsMerelyYamlShapedIsRefused(t *testing.T) {
	h := testHome(t)
	path := filepath.Join(h.harness(), processName)

	// a corrupted registry that reads as "no fixtures were registered" would skip
	// the restarts a resumed run depends on, quietly, instead of invalidating the
	// run at the harness's tier.
	require.NoError(t, os.WriteFile(path, []byte("unrelated: true\n"), 0o644))
	_, err := readProcessRegistry(path)
	assert.Error(t, err)
}

func TestAShutdownStatusIsNotACrash(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--exit-on-stop 3")
	f.StopTimeout = 5 * time.Second
	w.Start(f)

	// a shutdown path is entitled to return whatever it likes. the harness asked it
	// to stop and it stopped; calling that the product's highest-order failure would
	// be the crash tier triggered by the harness's own hand.
	w.Quiesce()
	assert.Empty(t, cur.Findings)
	assert.Equal(t, 0, w.run.Count(ClassCrash))
}

func TestAnExitObservedBeforeItWasAskedForIsUnsolicited(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--exit-on-stop 0")
	w.Start(f)

	// exiting zero on its own is still a fixture that stopped being there. the test
	// is when the exit was observed, not what status it carried — and an exit
	// observed before anything was asked for cannot have been caused by the asking.
	inst := w.instances["server"]
	require.NoError(t, inst.cmd.Process.Signal(unix.SIGTERM))
	<-inst.done
	assert.True(t, inst.exitedAt.Before(time.Now()))

	// the wait has already returned, so the boundary meets it on the already-dead
	// path — which is the same statement by another route.
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "at its boundary")
}

func TestAnUnreadableReadinessFileIsAFault(t *testing.T) {
	w, _, h := testW(t)

	// only absence means the product has not written it yet. a path the harness
	// cannot inspect at all is the harness's problem, and waiting out the timeout
	// would turn an unreadable world into a claim about the product.
	sealed := filepath.Join(h.world(), "sealed")
	require.NoError(t, os.MkdirAll(sealed, 0o755))
	require.NoError(t, os.Chmod(sealed, 0o000))
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })

	f := Fixture{Name: "server", Literal: "toy sleep 30s", ReadyFile: "sealed/ready", ReadyTimeout: 10 * time.Second}
	started := time.Now()
	unwound, class := capture(func() { w.Start(f) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Less(t, time.Since(started), 5*time.Second, "it says so at once rather than waiting out the timeout")
}

func TestACrashWindowPublishesNoSavePoint(t *testing.T) {
	w, cur, _ := testW(t)

	// control flow normally ends the invocation before publication is reached, but
	// the checkpoint seam does not get to depend on control flow for a guarantee it
	// states outright.
	capture(func() { w.Run("toy panic") })
	require.Len(t, cur.Findings, 1)
	require.Equal(t, ClassCrash, cur.Findings[0].Class)

	unwound, class := capture(func() { w.publishBoundary(1, "durability") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Contains(t, cur.Findings[1].Message, "publishes no save point")

	refs, err := newCheckpoints(w.home.checkpointsDir()).list()
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestCleanupSurvivesUnreadableEvidence(t *testing.T) {
	w, cur, _ := testW(t)
	first, _ := serveFixture(t, "first")
	second, _ := serveFixture(t, "second")
	w.Start(first)
	w.Start(second)

	// once the run is unwinding a fault is recorded rather than unwinding again, so
	// every step past one has to be a step the code chose to take. losing the
	// evidence for one fixture must not abandon the cleanup of the next.
	require.NoError(t, os.Remove(filepath.Join(w.home.logs(), "second.log")))
	w.shutdown()

	require.NotEmpty(t, cur.Findings)
	assert.Equal(t, ClassFault, cur.Findings[0].Class)
	assert.Contains(t, cur.Findings[0].Message, "reading the log")
	assert.Empty(t, w.instances, "both fixtures were still stopped")
}

func TestAnInterruptionDoesNotEraseACrashItDidNotCause(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--panic-after 200ms")
	w.Start(f)
	time.Sleep(600 * time.Millisecond)

	// what provenance excludes is a termination the harness initiated, and that
	// exclusion is already made per instance by the signals the fixture was
	// actually sent. a fixture that came apart of its own accord before the
	// interrupt earned its finding, and discovering it during an interrupted
	// cleanup is not a reason to throw the evidence away.
	w.cancelled.Store(true)
	w.shutdown()

	assert.Equal(t, 1, w.run.Count(ClassCrash))
	require.NotEmpty(t, cur.Findings)
	assert.Contains(t, cur.Findings[0].Detail, "panic:")
}

func TestAMarkerSurvivesAFixtureThatWillNotStop(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--marker-after 200ms", "--slow-stop 30s")
	f.StopTimeout = 700 * time.Millisecond
	w.Start(f)
	time.Sleep(600 * time.Millisecond)

	// two things are true and both have to land: the product's own account of
	// coming apart, and the harness's inability to close the world. reading the
	// window consumes it, so evidence not spent here is lost for good.
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)

	require.Len(t, cur.Findings, 2)
	assert.Equal(t, ClassCrash, cur.Findings[0].Class)
	assert.Contains(t, cur.Findings[0].Detail, "panic:")
	assert.Equal(t, ClassFault, cur.Findings[1].Class)
	assert.Equal(t, 2, w.run.Verdict(), "the harness could not close the world, so the run is invalid")
}

func TestABoundaryRefusesDivergentHarnessState(t *testing.T) {
	w, cur, h := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)
	w.Put("old-snap", "9f2c1ab77e40")
	w.Quiesce()

	// the harness's own deposits and registry live inside the world, which means a
	// misbehaving product can reach them. faithful bytes are not enough if those
	// bytes disagree with the run that produced them: this run would go on
	// restarting the fixtures it remembers while a resumed run reloads the altered
	// file and stands up a different world.
	require.NoError(t, os.WriteFile(filepath.Join(h.harness(), kvName),
		[]byte("values:\n  old-snap: tampered\n"), 0o644))

	unwound, class := capture(func() { w.publishBoundary(1, "durability") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Contains(t, cur.Findings[0].Message, "no longer match")

	refs, err := newCheckpoints(h.checkpointsDir()).list()
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestTheExitProbeAnswersBeforeTheWaitDoes(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	inst := w.instances["server"]
	gone, err := alreadyExited(inst.cmd.Process.Pid)
	require.NoError(t, err)
	assert.False(t, gone)

	// the harness's own wait tells it a process is gone only once the wait returns,
	// which can be well after the death. the probe has to answer at the moment it is
	// asked, and it has to keep answering for a zombie — that is the whole reason
	// provenance can be settled at all.
	require.NoError(t, inst.cmd.Process.Signal(unix.SIGKILL))
	deadline := time.Now().Add(2 * time.Second)
	for {
		gone, err = alreadyExited(inst.cmd.Process.Pid)
		require.NoError(t, err)
		if gone {
			break
		}
		require.True(t, time.Now().Before(deadline), "the probe never saw the death")
		time.Sleep(time.Millisecond)
	}
	assert.True(t, gone)

	// and the boundary reads it as the unsolicited death it was.
	unwound, class := capture(func() { w.Quiesce() })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
}

func TestABaseUrlThatAbsorbsItsPathsIsAFault(t *testing.T) {
	w, _, _ := testW(t)

	// a base is something paths get appended to. a query or a fragment absorbs
	// whatever follows it, so every request would be well-formed and aimed
	// somewhere the suite never named — and the readiness timeout would eventually
	// blame the product for it.
	for name, base := range map[string]string{
		"fragment":     "http://127.0.0.1:9000#anchor",
		"query":        "http://127.0.0.1:9000?trace=1",
		"forced query": "http://127.0.0.1:9000?",
	} {
		f, _ := serveFixture(t, "server")
		f.BaseURL = base
		unwound, class := capture(func() { w.Start(f) })
		assert.True(t, unwound, name)
		assert.Equal(t, ClassFault, class, name)
	}
}
