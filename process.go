package challenge

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/michaelquigley/df/dd"
	"github.com/michaelquigley/df/dl"
	"golang.org/x/sys/unix"
)

// processName is the fixture registry inside the checkpoint image.
const processName = "processes.yaml"

const (
	// defaultReadyTimeout bounds how long a fixture may take to answer.
	defaultReadyTimeout = 30 * time.Second
	// defaultStopTimeout bounds a clean shutdown. quiescence assumes clean closes:
	// a process holding a lock and a write-ahead log has to release them, and a
	// world snapshotted around a process that had to be killed is not a closed one.
	defaultStopTimeout = 15 * time.Second
	// readyInterval is how often a starting fixture is probed.
	readyInterval = 50 * time.Millisecond
	// reapGrace bounds the wait for a process the harness has already killed. an
	// unreapable child is a cleanup failure, not something to hang on.
	reapGrace = 5 * time.Second
)

// Fixture declares a long-lived process the world supervises: started with a
// readiness probe, its output captured, and bounced at every challenge boundary.
type Fixture struct {
	// Name identifies the fixture in the registry, in its log file, and in every
	// finding attributed to it.
	Name string
	// Literal is the command as a user would type it, with {} placeholders.
	Literal string
	// Env overrides environment variables for this fixture.
	Env map[string]string
	// Dir runs the fixture somewhere other than the world root.
	Dir string
	// BaseURL is where this fixture answers the wire channel.
	BaseURL string
	// ReadyURL is a path under BaseURL that must answer for the fixture to count
	// as ready. answering means answering healthily — a success or a redirect. a
	// process that replies "not yet", or that does not recognize the path the
	// suite named, is live rather than ready.
	ReadyURL string
	// ReadyFile is a path under the world that must appear.
	ReadyFile string
	// ReadyTimeout bounds becoming ready; zero takes the default.
	ReadyTimeout time.Duration
	// StopTimeout bounds a clean shutdown; zero takes the default.
	StopTimeout time.Duration
}

// fixtureSpec is a registered fixture as it persists: the resolved argv and
// everything a restart needs. it lives inside the checkpoint image, so a resumed
// run knows what to bring back up and a restore never leaves a run starting a
// fixture that a later challenge registered.
type fixtureSpec struct {
	Name         string
	Argv         []string
	Env          map[string]string
	Dir          string
	BaseURL      string
	ReadyURL     string
	ReadyFile    string
	ReadyTimeout time.Duration
	StopTimeout  time.Duration
}

// processFile is the on-disk shape of the fixture registry. the field is required
// so a document that is merely YAML-shaped cannot bind as an empty registry: a
// corrupted registry that reads as "no fixtures were registered" would skip the
// restarts a resumed run depends on, quietly, instead of invalidating the run.
type processFile struct {
	Fixtures []fixtureSpec `dd:"+required"`
}

// instance is one live run of a fixture. it is session state, never world state:
// pids and log offsets describe this process, not the world at a boundary.
type instance struct {
	spec    fixtureSpec
	cmd     *exec.Cmd
	logPath string
	// offset is where this instance's output window begins. each start records the
	// log's current size, so a boundary scan reads only what this instance wrote
	// and an old panic is never re-attributed by a later boundary or a resumed run.
	offset int64
	done   chan struct{}
	// exitedAt is when the harness's own wait returned, and signalledAt is when it
	// first asked the fixture to stop. an exit observed before anything was asked
	// for cannot have been caused by the asking.
	exitedAt    time.Time
	signalledAt time.Time
	// waitErr is whatever supervising this child ended with. the child's own exit
	// is product evidence; anything else is the harness losing track of a process.
	waitErr error
	// killedByHarness records that the harness asked this process to stop, so its
	// death is expected rather than evidence of anything.
	killedByHarness bool
	// sentSignals is every signal the harness delivered to this instance. it is
	// what makes provenance a statement about the manner of a death rather than
	// about the ordering of two observations.
	sentSignals []unix.Signal
	// crashReported records that this instance's death has already been attributed,
	// so one crash stays one finding however many surfaces observe it.
	crashReported bool
}

// exited reports whether the process has already gone.
func (i *instance) exited() bool {
	select {
	case <-i.done:
		return true
	default:
		return false
	}
}

// Start supervises a long-lived process and waits for it to become usable.
//
// the three ways starting can fail are three different statements. a spawn failure
// or an invalid probe declaration is the harness or the suite being broken — a
// harness fault. a process that dies while starting is a product crash. one that
// lives but never answers is a product-surface break. and both product-caused arms
// end the invocation: one honest finding beats a corridor of cascade run against a
// fixture that does not exist.
func (w *W) Start(f Fixture, args ...any) {
	spec := w.buildSpec(f, args)
	if existing, ok := w.instances[spec.Name]; ok {
		if !existing.exited() {
			// replacing the registration would strand the running process outside
			// both the registry and the instance table, where quiescence cannot
			// reach it — and a live writer nobody can stop is exactly what a
			// boundary snapshot must never be taken around.
			w.faultf("fixture %q is already running; a corridor stops a fixture at its boundary rather than declaring it twice", spec.Name)
		}
		// it died before anyone asked it to, and replacing it would bury both the
		// death and the window that explains it.
		trusted := w.requireCleanWait(existing)
		evidence := w.scanWindow(existing)
		if trusted {
			w.crashFromDeath(existing,
				fmt.Sprintf("fixture %q was found dead when it was declared again", spec.Name),
				joinEvidence(w.exitEvidence(existing), evidence))
		}
		delete(w.instances, spec.Name)
	}
	w.registerSpec(spec)
	w.step(StepNote, fmt.Sprintf("start %s", spec.Name), strings.Join(spec.Argv, " "))
	w.launch(spec)
}

// buildSpec validates a declaration and resolves it into something a restart can
// reproduce.
func (w *W) buildSpec(f Fixture, args []any) fixtureSpec {
	if f.Name == "" {
		w.faultf("a fixture needs a name")
	}
	if f.ReadyURL == "" && f.ReadyFile == "" {
		// a supervised process nobody can tell is ready is a fixture nothing can
		// depend on, and the corridor beneath it would be racing its startup.
		w.faultf("fixture %q declares no readiness probe", f.Name)
	}
	if f.ReadyURL != "" && f.BaseURL == "" {
		w.faultf("fixture %q probes %q but declares no base URL to probe it against", f.Name, f.ReadyURL)
	}
	if f.ReadyTimeout < 0 || f.StopTimeout < 0 {
		w.faultf("fixture %q declares a negative timeout", f.Name)
	}
	spec := fixtureSpec{
		Name: f.Name,
		Argv: w.resolveArgv(f.Literal, args),
		// the registry is world state that rides a checkpoint. holding the caller's
		// map would let a later mutation move the in-memory topology while the
		// checkpointed image stayed where it was, so a full run and a resumed run
		// could start the same fixture with different environments.
		Env:          ownedEnv(f.Env),
		Dir:          f.Dir,
		BaseURL:      strings.TrimSuffix(f.BaseURL, "/"),
		ReadyURL:     f.ReadyURL,
		ReadyFile:    f.ReadyFile,
		ReadyTimeout: f.ReadyTimeout,
		StopTimeout:  f.StopTimeout,
	}
	if spec.ReadyTimeout == 0 {
		spec.ReadyTimeout = defaultReadyTimeout
	}
	if spec.StopTimeout == 0 {
		spec.StopTimeout = defaultStopTimeout
	}
	// a probe the harness can never issue would spend the whole readiness timeout
	// failing and then report that the product never became ready — a claim about
	// the product for a declaration the suite got wrong.
	if err := validateProbe(spec); err != nil {
		w.faultf("fixture %q: %v", f.Name, err)
	}
	// resolving the binary here means a missing one faults at declaration rather
	// than at the restart three boundaries later.
	w.resolveBinary(spec.Argv[0])
	return spec
}

// registerSpec records a fixture in the registry, replacing an earlier declaration
// of the same name.
func (w *W) registerSpec(spec fixtureSpec) {
	replaced := false
	for i, existing := range w.specs {
		if existing.Name == spec.Name {
			w.specs[i], replaced = spec, true
			break
		}
	}
	if !replaced {
		w.specs = append(w.specs, spec)
	}
	if err := writeProcessRegistry(filepath.Join(w.home.harness(), processName), w.specs); err != nil {
		w.faultf("registering fixture %q: %v", spec.Name, err)
	}
}

// launch spawns one instance of a spec and waits for it to become usable.
func (w *W) launch(spec fixtureSpec) {
	logPath := filepath.Join(w.home.logs(), safeName(spec.Name)+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		w.faultf("preparing logs for fixture %q: %v", spec.Name, err)
	}
	offset := int64(0)
	if info, err := os.Stat(logPath); err == nil {
		offset = info.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		w.faultf("reading the log for fixture %q: %v", spec.Name, err)
	}
	sink, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.faultf("opening the log for fixture %q: %v", spec.Name, err)
	}

	cmd := exec.Command(w.resolveBinary(spec.Argv[0]), spec.Argv[1:]...)
	cmd.Dir = w.home.world()
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	cmd.Env = w.childEnv(spec.Env)
	cmd.Stdout, cmd.Stderr = sink, sink
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	startErr := cmd.Start()
	// the child holds its own descriptor from here, so the parent keeps none.
	sink.Close()
	if startErr != nil {
		w.faultf("starting fixture %q: %v", spec.Name, startErr)
	}

	inst := &instance{spec: spec, cmd: cmd, logPath: logPath, offset: offset, done: make(chan struct{})}
	go func() {
		inst.waitErr = cmd.Wait()
		inst.exitedAt = time.Now()
		close(inst.done)
	}()
	w.instances[spec.Name] = inst
	dl.Debugf("started fixture %s as pid %d", spec.Name, cmd.Process.Pid)

	w.awaitReady(inst)
}

// awaitReady polls a starting fixture until it answers, dies, or runs out of time.
func (w *W) awaitReady(inst *instance) {
	deadline := time.Now().Add(inst.spec.ReadyTimeout)
	for {
		// fixtures deliberately do not die by an automatic context kill, so this is
		// the synchronous path that has to notice an interruption. waiting out the
		// readiness timeout would make a product claim — "never became ready" — for
		// a startup the harness itself cut short.
		if w.interrupted() {
			w.abandon()
		}
		if inst.exited() {
			trusted := w.requireCleanWait(inst)
			evidence := w.scanWindow(inst)
			if trusted {
				w.crashFromDeath(inst,
					fmt.Sprintf("fixture %q died while starting", inst.spec.Name),
					joinEvidence(w.exitEvidence(inst), evidence))
			}
			return
		}
		if w.probeReady(inst) {
			dl.Debugf("fixture %s is ready", inst.spec.Name)
			return
		}
		if time.Now().After(deadline) {
			w.record(ClassBreak,
				fmt.Sprintf("fixture %q was live but never became ready within %s", inst.spec.Name, inst.spec.ReadyTimeout),
				w.tail(inst))
			return
		}
		time.Sleep(readyInterval)
	}
}

// probeReady asks a fixture whether it is usable yet.
func (w *W) probeReady(inst *instance) bool {
	if inst.spec.ReadyFile != "" {
		// only absence means the product has not written it yet. a path the harness
		// cannot inspect at all is the harness's problem, and waiting out the
		// timeout would turn an unreadable world into a claim about the product.
		if _, err := os.Stat(w.Path(inst.spec.ReadyFile)); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				w.faultf("checking the readiness file for fixture %q: %v", inst.spec.Name, err)
			}
			return false
		}
	}
	if inst.spec.ReadyURL == "" {
		return true
	}
	client := newHTTPClient(2 * time.Second)
	resp, err := client.Get(inst.spec.BaseURL + inst.spec.ReadyURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode < 400
}

// Quiesce stops every supervised process, in reverse start order.
//
// the bounce is not a workaround. it is load-bearing twice over: no snapshot is
// ever taken under a live writer holding open state, so a copy cannot capture a
// torn store; and a resumed run reaches every challenge through the same bounce a
// full run does, so the two can never disagree about the world a challenge starts
// in. it is a standing pressure test in its own right — the system under test must
// survive its operating process restarting, every boundary, every run.
func (w *W) Quiesce() {
	for i := len(w.specs) - 1; i >= 0; i-- {
		inst, ok := w.instances[w.specs[i].Name]
		if !ok {
			continue
		}
		w.quiesce(inst)
		delete(w.instances, inst.spec.Name)
	}
}

// Restart brings every registered fixture back up, in start order.
func (w *W) Restart() {
	for _, spec := range w.specs {
		if _, live := w.instances[spec.Name]; live {
			continue
		}
		w.launch(spec)
	}
}

// quiesce stops one instance and reads what it wrote while it was up.
func (w *W) quiesce(inst *instance) {
	pid := inst.cmd.Process.Pid

	// asked at the moment it matters, rather than inferred from a wait that returns
	// whenever it returns. a fixture that fell over just before its boundary must
	// not be able to be signalled, look stopped by that signal, and lose the crash
	// it earned.
	gone, err := alreadyExited(pid)
	if err != nil {
		w.faultf("checking whether fixture %q is still running: %v", inst.spec.Name, err)
		return
	}
	if gone || inst.exited() {
		<-inst.done
		trusted := w.requireCleanWait(inst)
		// the leader is gone, but its group may not be. anything it started can
		// still be writing into the world, so the group is cleared before anything
		// is said about the death.
		evidence := w.scanWindow(inst)
		if !clearGroup(pid, inst.spec.StopTimeout) {
			w.faultf("fixture %q left processes behind that would not stop; the world it left is not a closed one", inst.spec.Name)
		}
		// found dead before its requested quiesce: a crash event in its own right,
		// marker or no marker.
		if trusted {
			w.crashFromDeath(inst,
				fmt.Sprintf("fixture %q was already dead at its boundary", inst.spec.Name),
				joinEvidence(w.exitEvidence(inst), evidence))
		}
		return
	}

	// provenance is claimed only for a signal that actually landed. recording it
	// first would let a fixture that died of its own SIGTERM a moment earlier be
	// credited to the harness, and the crash it earned would disappear.
	inst.signalledAt = time.Now()
	if err := killGroup(inst.cmd.Process, unix.SIGTERM); err != nil {
		if !errors.Is(err, unix.ESRCH) {
			// the harness cannot stop it, so there is nothing further to wait for.
			w.faultf("signalling fixture %q: %v", inst.spec.Name, err)
			return
		}
	} else {
		inst.killedByHarness = true
		inst.sentSignals = append(inst.sentSignals, unix.SIGTERM)
	}

	deadline := time.Now().Add(inst.spec.StopTimeout)
	stopped := true
	select {
	case <-inst.done:
	case <-time.After(time.Until(deadline)):
		w.escalate(inst)
		select {
		case <-inst.done:
		case <-time.After(reapGrace):
			// a child that survives SIGKILL, or that cannot be reaped, is a cleanup
			// failure. hanging here would leave the run neither finished nor
			// reported, which is the one outcome a guardian suite must not have.
			w.faultf("fixture %q could not be reaped %s after it was killed; the world it left is not a closed one",
				inst.spec.Name, reapGrace)
		}
		stopped = false
	}

	// the leader exiting is not the same as the fixture being gone. a process it
	// started can outlive it and keep writing into the world, and a snapshot taken
	// around that writer would be exactly the torn image the bounce exists to
	// prevent — so quiescence means the whole group, not just the one process the
	// harness happens to hold a handle to.
	if stopped {
		for !groupGone(pid) {
			if time.Now().After(deadline) {
				w.escalate(inst)
				stopped = false
				break
			}
			time.Sleep(readyInterval)
		}
	}

	trusted := w.requireCleanWait(inst)
	// the window is read before the fault, so a product that panicked on its way
	// down is still on the record even when the harness has to give up on it. the
	// read consumes the window, so the evidence has to be spent here or lost.
	evidence := w.scanWindow(inst)
	if !stopped {
		// two things are true and both have to land: the product's own account of
		// coming apart, and the harness's inability to close the world. the crash is
		// recorded without unwinding so the fault that ends the run can follow it.
		if evidence != "" {
			w.crashQuietly(inst, fmt.Sprintf("fixture %q crashed", inst.spec.Name), evidence)
		}
		// a fixture that will not close cleanly invalidates the run. the world it
		// leaves behind may hold a torn store or a lock nobody released, and a
		// snapshot of it would be a lie told quietly.
		w.faultf("fixture %q did not stop within %s and had to be killed; the world it left is not a closed one",
			inst.spec.Name, inst.spec.StopTimeout)
		return
	}
	// a stop the harness asked for explains an exit of zero, or a death by a signal
	// the harness actually sent. anything else is a death the fixture chose,
	// whatever the ordering of observations looked like from outside — and a
	// fixture that fell over just before its boundary must not be able to pass for
	// one that closed when it was asked.
	if trusted && !solicitedExit(inst) {
		w.crashInstance(inst,
			fmt.Sprintf("fixture %q ended on its own terms at its boundary", inst.spec.Name),
			joinEvidence(w.exitEvidence(inst), evidence))
	}
	// a marker in the window is the product's own account of coming apart, and it
	// counts whoever asked the process to stop.
	if evidence != "" {
		w.crashFromMarker(inst, evidence)
	}
}

// escalate delivers SIGKILL to a fixture's group, claiming provenance only for a
// signal that landed.
func (w *W) escalate(inst *instance) {
	if err := killGroup(inst.cmd.Process, unix.SIGKILL); err == nil {
		inst.killedByHarness = true
		inst.sentSignals = append(inst.sentSignals, unix.SIGKILL)
	}
}

// clearGroup terminates whatever a dead fixture left behind and reports whether the
// group emptied.
//
// no provenance is claimed here: the leader is already gone, so this signal reaches
// only what it started and says nothing about how the leader itself died.
func clearGroup(pgid int, within time.Duration) bool {
	if groupGone(pgid) {
		return true
	}
	_ = unix.Kill(-pgid, unix.SIGKILL)
	deadline := time.Now().Add(within)
	for !groupGone(pgid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(readyInterval)
	}
	return true
}

// solicitedExit reports whether the harness explains how a fixture ended.
//
// three things can settle it, in order. an exit the harness observed before it
// ever asked for one was not its doing, whatever else is true — that test is
// one-directional and therefore safe: the wait returns after the process is
// already gone, so an earlier observation proves an earlier death. a death by
// signal is the harness's only if the harness sent that signal. and a normal exit
// is explained by the request itself rather than by its status, because a shutdown
// path is entitled to return whatever it likes; a product that comes apart on its
// way down says so with a marker, and that arm is read separately.
//
// what remains unsettled is a fixture that exits quietly in the instant between
// the last look and the signal, and whose wait happens not to return until after.
// nothing outside the kernel can close that window by watching for it.
func solicitedExit(inst *instance) bool {
	state := inst.cmd.ProcessState
	if state == nil {
		return true
	}
	if !inst.signalledAt.IsZero() && inst.exitedAt.Before(inst.signalledAt) {
		return false
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return slices.Contains(inst.sentSignals, unix.Signal(status.Signal()))
	}
	return inst.killedByHarness
}

// groupGone reports whether a fixture's process group holds nothing at all. every
// harness-spawned process leads its own group, so this asks about that fixture and
// its descendants and about nothing else.
func groupGone(pgid int) bool {
	return errors.Is(unix.Kill(-pgid, 0), unix.ESRCH)
}

// publishBoundary snapshots the world as a challenge's save point.
//
// it lives here rather than beside the copy machinery because a snapshot is only
// truthful against a *closed* world, and that has to be a property of the
// operation rather than a rule a caller remembers. a supervised process still
// holding open state can be mid-write while the copy walks the tree, and the
// resulting image would be a torn store that nothing downstream could tell from a
// real one.
func (w *W) publishBoundary(boundary int, name string) (checkpointRef, error) {
	// a crash or a break in this challenge's window makes it checkpoint-ineligible,
	// and that has to be a property of the operation. control flow normally ends the
	// invocation before publication is reached, but the checkpoint seam is not
	// allowed to depend on control flow for a guarantee it states outright.
	for _, f := range w.cur.Findings {
		if f.Class.Terminal() {
			w.faultf("refusing to snapshot boundary %d: challenge %q recorded a %s, and its window publishes no save point",
				boundary, w.cur.Name, f.Class)
			// faultf returns rather than unwinding once the run is already on its
			// way out, and a refusal that then published anyway would be the one
			// thing this operation exists to make impossible.
			return checkpointRef{}, fmt.Errorf("boundary %d is not eligible for a save point", boundary)
		}
	}
	if outstanding := w.unquiescedFixtures(); len(outstanding) > 0 {
		w.faultf("refusing to snapshot boundary %d while %s not been through a boundary; a checkpoint is only truthful against a closed world",
			boundary, strings.Join(outstanding, ", "))
		return checkpointRef{}, fmt.Errorf("boundary %d cannot be snapshotted against an open world", boundary)
	}
	if err := w.requireHarnessStateIntact(); err != nil {
		w.faultf("refusing to snapshot boundary %d: %v", boundary, err)
		return checkpointRef{}, fmt.Errorf("boundary %d cannot be snapshotted from divergent harness state: %w", boundary, err)
	}
	return newCheckpoints(w.home.checkpointsDir()).publish(boundary, name, w.run.SessionId, w.run.RunId, w.home.world())
}

// requireHarnessStateIntact checks that what is about to be copied still says what
// the run believes.
//
// the harness's own deposits, world environment, and fixture registry live inside
// the world, which means a misbehaving product can reach them. faithful bytes are
// not enough if those bytes disagree with the run that produced them: the current
// run would go on restarting the fixtures it remembers while a resumed run reloads
// the altered file and stands up a different world. the two must not be able to
// disagree about the same boundary.
func (w *W) requireHarnessStateIntact() error {
	kv, err := readHarnessMap(filepath.Join(w.home.harness(), kvName))
	if err != nil {
		return err
	}
	if !maps.Equal(kv, w.kv) {
		return errors.New("the deposits on disk no longer match the ones this run made")
	}
	env, err := readHarnessMap(filepath.Join(w.home.harness(), envName))
	if err != nil {
		return err
	}
	if !maps.Equal(env, w.env) {
		return errors.New("the world environment on disk no longer matches the one this run set")
	}
	specs, err := readProcessRegistry(filepath.Join(w.home.harness(), processName))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(specs, w.specs) {
		return errors.New("the fixture registry on disk no longer matches the fixtures this run declared")
	}
	return nil
}

// unquiescedFixtures names every supervised process that has not been through a
// boundary, in start order.
//
// the test is presence in the table, not liveness. quiescence is what removes an
// instance, and it is also where an unsolicited death is observed and classified —
// so a fixture that died quietly and is merely *gone* has not been accounted for
// yet, and publishing around it would make a checkpoint selectable from a crash
// window nobody has looked at.
func (w *W) unquiescedFixtures() []string {
	var outstanding []string
	for _, spec := range w.specs {
		if _, ok := w.instances[spec.Name]; ok {
			outstanding = append(outstanding, fmt.Sprintf("fixture %q has", spec.Name))
		}
	}
	return outstanding
}

// shutdown quiesces everything on the way out of a run, best-effort.
//
// the unwind is unconditional: a run that ends for any reason — clean, findings,
// harness fault, interruption — stops its processes before it exits. the residue a
// failed run leaves is only a debugging session if nothing is still writing to it.
func (w *W) shutdown() {
	w.unwinding = true
	w.Quiesce()
	// cleanup is where a deferred wire failure learns what it was.
	w.resolvePendingBreaks()
}

// scanWindow reads what an instance wrote since its window opened and reports any
// crash marker in it, advancing the offset so the same output is never read twice.
func (w *W) scanWindow(inst *instance) string {
	// the log is harness-owned state, created when the fixture launched. finding it
	// gone means the harness can no longer inspect the evidence it promised to
	// capture, and reporting "no evidence" would let a shutdown panic disappear.
	f, err := os.Open(inst.logPath)
	if err != nil {
		// faultf returns rather than unwinding once the run is already on its way
		// out, so every step past a fault has to be one the code chose to take.
		w.faultf("reading the log for fixture %q: %v", inst.spec.Name, err)
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(inst.offset, io.SeekStart); err != nil {
		w.faultf("reading the log for fixture %q: %v", inst.spec.Name, err)
		return ""
	}
	window, err := io.ReadAll(f)
	if err != nil {
		w.faultf("reading the log for fixture %q: %v", inst.spec.Name, err)
		return ""
	}
	inst.offset += int64(len(window))
	if marker := findMarker(string(window)); marker != "" {
		return fmt.Sprintf("log carries %q:\n%s", marker, lastLines(string(window), 20))
	}
	return ""
}

// tail reports the end of an instance's window without consuming it, for the
// findings that want context rather than evidence.
func (w *W) tail(inst *instance) string {
	data, err := os.ReadFile(inst.logPath)
	if err != nil {
		w.faultf("reading the log for fixture %q: %v", inst.spec.Name, err)
		return ""
	}
	if int64(len(data)) > inst.offset {
		data = data[inst.offset:]
	}
	return lastLines(string(data), 20)
}

// exitEvidence describes how a dead instance went.
func (w *W) exitEvidence(inst *instance) string {
	state := inst.cmd.ProcessState
	if state == nil {
		return ""
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return fmt.Sprintf("terminated by signal %s", status.Signal())
	}
	return fmt.Sprintf("exited %d", state.ExitCode())
}

// requireCleanWait faults when the harness failed to supervise its own child, and
// reports whether what it observed can be trusted.
//
// a wait that ends in the child's own exit — however unhappy — is product
// evidence. a wait that ends any other way is the harness losing track of a
// process it started, and reporting that as a crash would blame the product for
// the harness's blindness.
func (w *W) requireCleanWait(inst *instance) bool {
	if inst.waitErr == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(inst.waitErr, &exitErr) {
		return true
	}
	w.faultf("supervising fixture %q: %v", inst.spec.Name, inst.waitErr)
	// faultf returns rather than unwinding once the run is on its way out, and the
	// caller must not go on to blame the product for what the harness could not see.
	return false
}

// crashFromDeath reports a fixture that died on its own.
//
// a termination the harness itself asked for is not evidence of anything: the
// crash tier is only worth having if it cannot be triggered by the harness's own
// hand.
func (w *W) crashFromDeath(inst *instance, message, evidence string) {
	if inst.killedByHarness {
		return
	}
	w.crashInstance(inst, message, evidence)
}

// crashFromMarker reports a panic the product printed.
//
// this arm is deliberately not gated on provenance. who asked a process to stop
// has no bearing on whether it came apart while stopping, and a fixture that
// panics in its shutdown path is a product failure that would otherwise pass for
// a clean close.
func (w *W) crashFromMarker(inst *instance, evidence string) {
	w.crashInstance(inst, fmt.Sprintf("fixture %q crashed", inst.spec.Name), evidence)
}

// crashInstance records one crash finding for a fixture, at most once.
//
// evidence coalesces: a death observed at a boundary, in a log window, and by a
// refused request is one crash and earns one finding.
//
// an interruption does not silence this. what provenance excludes is a termination
// the harness itself initiated, and that exclusion is already made per instance by
// the signals it was actually sent — a fixture that panicked of its own accord a
// moment before the interrupt earned its finding, and discovering it during an
// interrupted cleanup is not a reason to throw the evidence away.
func (w *W) crashInstance(inst *instance, message, evidence string) {
	if w.markCrashed(inst) {
		w.record(ClassCrash, message, evidence)
	}
}

// crashQuietly records a fixture's crash without unwinding, for the paths where a
// harness fault is about to end the run and both statements have to survive.
func (w *W) crashQuietly(inst *instance, message, evidence string) {
	if w.markCrashed(inst) {
		w.recordQuiet(ClassCrash, message, evidence)
	}
}

// markCrashed records that an instance's collapse has been attributed, and reports
// whether this is the first time. one crash stays one finding however many surfaces
// observe it.
func (w *W) markCrashed(inst *instance) bool {
	if inst.crashReported {
		return false
	}
	inst.crashReported = true
	w.crashedFixtures[inst.spec.Name] = true
	return true
}

// joinEvidence assembles the non-empty pieces of a crash's account.
func joinEvidence(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "; ")
}

// lastLines keeps the tail of a body, which is where a failure usually is.
func lastLines(body string, n int) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// readProcessRegistry loads the registered fixtures, treating absence as none
// registered and anything else as harness-owned state the harness cannot trust.
func readProcessRegistry(path string) ([]fixtureSpec, error) {
	f, err := dd.NewYAMLFile[processFile](path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the fixture registry %s: %w", path, err)
	}
	// binding says the document was shaped like a registry, not that it describes
	// fixtures anything can start. an entry that would come apart at the restart is
	// corrupted harness-owned state, and it has to leave through the harness's tier
	// rather than as an uncontrolled panic that would read like the harness itself
	// crashing.
	seen := map[string]bool{}
	for i, spec := range f.Fixtures {
		switch {
		case spec.Name == "":
			return nil, fmt.Errorf("fixture %d in %s has no name", i, path)
		case seen[spec.Name]:
			return nil, fmt.Errorf("fixture %q appears twice in %s", spec.Name, path)
		case len(spec.Argv) == 0:
			return nil, fmt.Errorf("fixture %q in %s has no command", spec.Name, path)
		case spec.ReadyURL == "" && spec.ReadyFile == "":
			return nil, fmt.Errorf("fixture %q in %s declares no readiness probe", spec.Name, path)
		case spec.ReadyURL != "" && spec.BaseURL == "":
			return nil, fmt.Errorf("fixture %q in %s probes a URL with no base to probe it against", spec.Name, path)
		case spec.ReadyTimeout <= 0 || spec.StopTimeout <= 0:
			return nil, fmt.Errorf("fixture %q in %s carries a non-positive timeout", spec.Name, path)
		}
		// a probe that cannot be issued has to fault here rather than spending the
		// readiness timeout failing and then blaming the product for never becoming
		// ready — and a malformed one arriving from the registry is no less the
		// harness's own state than one arriving from a declaration.
		if err := validateProbe(spec); err != nil {
			return nil, fmt.Errorf("fixture %q in %s: %w", spec.Name, path, err)
		}
		seen[spec.Name] = true
		// an absent map and an empty one are the same declaration, and the two
		// spellings must not read as a divergence between disk and memory.
		f.Fixtures[i].Env = ownedEnv(spec.Env)
	}
	return f.Fixtures, nil
}

// ownedEnv copies a declaration's environment into a map the registry owns, always
// present even when empty so disk and memory spell the same thing the same way.
func ownedEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

// validateProbe reports whether a fixture's wire declarations are ones the harness
// can actually issue — the base URL every request will be built on, and the
// readiness probe if it declares one.
func validateProbe(spec fixtureSpec) error {
	if spec.BaseURL != "" {
		if err := validateWireURL(spec.BaseURL); err != nil {
			return fmt.Errorf("base URL %s: %w", spec.BaseURL, err)
		}
		// a base is something paths get appended to. a query or a fragment absorbs
		// whatever follows it, so the request would be well-formed and aimed
		// somewhere the suite never named.
		parsed, err := url.Parse(spec.BaseURL)
		if err != nil {
			return fmt.Errorf("base URL %s: %w", spec.BaseURL, err)
		}
		if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return fmt.Errorf("base URL %s carries a query or fragment, which would absorb the paths appended to it", spec.BaseURL)
		}
	}
	if spec.ReadyURL == "" {
		return nil
	}
	if !strings.HasPrefix(spec.ReadyURL, "/") {
		return fmt.Errorf("readiness path %q is not absolute; it must begin with /", spec.ReadyURL)
	}
	if err := validateWireURL(spec.BaseURL + spec.ReadyURL); err != nil {
		return fmt.Errorf("readiness probe %s%s: %w", spec.BaseURL, spec.ReadyURL, err)
	}
	return nil
}

// validateWireURL reports whether a target is one the wire channel can issue.
func validateWireURL(target string) error {
	parsed, err := url.Parse(target)
	if err != nil {
		return err
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("not an http or https URL with a host")
	}
	return nil
}

// writeProcessRegistry persists the registered fixtures.
func writeProcessRegistry(path string, specs []fixtureSpec) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("preparing %s: %w", filepath.Dir(path), err)
	}
	if err := dd.UnbindYAMLFile(&processFile{Fixtures: specs}, path); err != nil {
		return fmt.Errorf("writing the fixture registry %s: %w", path, err)
	}
	return nil
}
