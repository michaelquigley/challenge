package challenge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/michaelquigley/df/dl"
)

// Options is how a face asks for a run.
type Options struct {
	// From names the challenge to resume from: the world is restored to the
	// greatest boundary strictly before it, the challenges in between are replayed,
	// and the corridor continues to the end.
	From string
	// Only names the single challenge to run, against its immediate predecessor's
	// boundary and nothing else.
	Only string
	// Clean discards the world generation and its residue, and does nothing else.
	Clean bool
	// WorldHome overrides the gauntlet's declared home. it is applied before the
	// lock is derived, so the lock, the artifacts, and the world all land together.
	WorldHome string
	// Transcript overrides where the run's narrative is written.
	Transcript string
	// Verbose asks the console reporter for every step rather than the salient ones.
	Verbose bool
	// Report is where the console reporter writes; nil means no console reporting.
	Report *consoleReporter
}

// engine sequences one invocation. it is the only owner of the boundary ordering,
// and the only thing that writes the run model.
type engine struct {
	ctx        context.Context
	g          Gauntlet
	opts       Options
	home       *home
	run        *Run
	w          *W
	transcript *transcriptWriter
	// pendingReport is a challenge whose console line waits for cleanup. a wire
	// failure defers its break until the fixture's fate is settled, so reporting
	// the challenge before then would print "ok" beside a finding the model is
	// about to record.
	pendingReport *ChallengeRun
}

// Execute runs a gauntlet and returns the model of what happened.
//
// it never returns an error: everything an invocation can fail at is a finding in
// the model, at the class the census gives it, so every face reads one thing and
// no face has to decide what a failure means.
//
// the sequence is fixed, and the lock is held across all of it: acquire the lock,
// run the consumer's bootstrap, reset or restore the world, then walk the corridor.
// nothing — not the bootstrap, not the world, not the artifacts — is touched before
// the lock is held, so a concurrent invocation is refused before it can disturb the
// run already in progress.
func Execute(ctx context.Context, g Gauntlet, opts Options) *Run {
	run := &Run{Gauntlet: g.Name, RunId: newId("r"), Started: time.Now()}
	defer func() { run.Ended = time.Now() }()

	if err := g.validate(); err != nil {
		runFault(run, "%v", err)
		return run
	}
	for _, c := range g.Challenges {
		run.Challenges = append(run.Challenges, &ChallengeRun{Name: c.Name, Doc: c.Doc, Status: StatusPending})
	}

	worldHome := g.WorldHome
	if opts.WorldHome != "" {
		worldHome = opts.WorldHome
	}
	h, err := newHome(worldHome, g.Name)
	if err != nil {
		runFault(run, "%v", err)
		return run
	}
	if err := os.MkdirAll(filepath.Dir(h.lockPath), 0o755); err != nil {
		runFault(run, "preparing %s: %v", filepath.Dir(h.lockPath), err)
		return run
	}

	lock, err := acquireLock(h.lockPath)
	if err != nil {
		runFault(run, "%v", err)
		return run
	}
	released := false
	defer func() {
		if !released {
			_ = lock.release()
		}
	}()

	if opts.Clean {
		if err := h.clean(); err != nil {
			runFault(run, "%v", err)
		}
		// releasing the lock is cleanup like any other, and cleanup that fails is a
		// harness fault rather than something a deferred call swallows on the way
		// out.
		if err := lock.release(); err != nil {
			runFault(run, "%v", err)
		}
		released = true
		return run
	}

	e := &engine{ctx: ctx, g: g, opts: opts, home: h, run: run}
	e.execute()

	// the model records its own interruption. a face that repaired this afterwards
	// would be a second owner of the model, and two consumers of the same engine
	// could then read different verdicts from the same cancellation.
	if ctx.Err() != nil {
		run.Interrupted = true
	}
	if err := lock.release(); err != nil {
		runFault(run, "%v", err)
	}
	released = true

	// the last word goes to the narrative, after everything that could still change
	// the verdict has. a transcript that said clean beside a model that says
	// otherwise would be a stale projection of the thing it exists to explain.
	e.writeTranscript()
	return run
}

// execute is the invocation proper, with the lock already held.
func (e *engine) execute() {
	if err := e.home.ensure(); err != nil {
		runFault(e.run, "%v", err)
		return
	}

	// the bootstrap gets a world handle focused on a record of its own, so what it
	// does is attributable rather than landing on whichever challenge came first.
	// the record only joins the model once a hook actually runs — a gauntlet with no
	// bootstrap should not carry one that says it executed.
	setup := &ChallengeRun{Name: "bootstrap", Status: StatusPending, Started: time.Now()}
	w, err := newW(e.ctx, e.home, e.run, setup)
	if err != nil {
		runFault(e.run, "%v", err)
		return
	}
	e.w = w
	transcript, err := newTranscriptWriter(e.transcriptPath())
	if err != nil {
		runFault(e.run, "%v", err)
		return
	}
	e.transcript = transcript

	// the unwind is unconditional. a run that ends for any reason stops its
	// processes before it exits, because the residue a failed run leaves is only a
	// debugging session if nothing is still writing to it.
	defer e.finish()

	if e.g.Bootstrap != nil {
		e.run.Bootstrap = setup
		setup.Status = StatusExecuted
		ok := e.guard(func() {
			// a bootstrap that issued commands is accountable for them the same way
			// a challenge is, and while the focus is still its own.
			defer e.w.resolvePending()
			if err := e.g.Bootstrap(w); err != nil {
				w.faultf("bootstrap: %v", err)
			}
		})
		if !ok {
			setup.Ended = time.Now()
			return
		}
		// core prescribes nothing about a bootstrap's content, so it may have started
		// a supervised process. the world has to be closed before it is reset or
		// restored beneath one — a snapshot taken around a live writer is the torn
		// image every part of this design exists to prevent.
		if !e.guard(func() { e.w.Quiesce() }) {
			setup.Ended = time.Now()
			return
		}
	}
	setup.Ended = time.Now()
	// the bootstrap is done being added to the moment the corridor takes over.
	setup.seal()

	first, last, ok := e.prepare()
	if !ok {
		return
	}
	for i := first; i <= last; i++ {
		if !e.runOne(i) {
			return
		}
	}
}

// prepare puts the world where the invocation means to start from, and reports the
// span of the corridor to execute.
func (e *engine) prepare() (first, last int, ok bool) {
	switch {
	case e.opts.From != "" && e.opts.Only != "":
		runFault(e.run, "--from and --only ask for different runs; name one")
		return 0, 0, false

	case e.opts.From != "":
		target, found := e.g.indexOf(e.opts.From)
		if !found {
			runFault(e.run, "no challenge named %q; the corridor is %s", e.opts.From, strings.Join(e.g.names(), ", "))
			return 0, 0, false
		}
		boundary, ok := e.restoreTo(target, resumeReplay)
		if !ok {
			return 0, 0, false
		}
		// everything between the restored boundary and the target is replayed live.
		// where an author traded a save point away, resume pays in replay rather
		// than in a full re-run — and replay is execution, not a lesser kind of it.
		return boundary + 1, len(e.g.Challenges), true

	case e.opts.Only != "":
		target, found := e.g.indexOf(e.opts.Only)
		if !found {
			runFault(e.run, "no challenge named %q; the corridor is %s", e.opts.Only, strings.Join(e.g.names(), ", "))
			return 0, 0, false
		}
		boundary, ok := e.restoreTo(target, resumeExact)
		if !ok {
			return 0, 0, false
		}
		// the rest of the corridor was not asked for, which is a different thing
		// from not being reached.
		for _, cr := range e.run.Challenges[target:] {
			cr.Status = StatusSkipped
		}
		return boundary + 1, target, true

	default:
		s, err := e.home.reset(e.g.corridor(), e.run.RunId)
		if err != nil {
			runFault(e.run, "%v", err)
			return 0, 0, false
		}
		e.run.SessionId = s.Id
		// the reset discarded the generation the earlier attempts belonged to, so
		// their narrative goes with it. a transcript carrying attempts against a
		// world that no longer exists is not a projection of anything.
		e.transcript.discardPrior()
		if err := e.w.reload(); err != nil {
			runFault(e.run, "%v", err)
			return 0, 0, false
		}
		return 1, len(e.g.Challenges), true
	}
}

// restoreTo navigates the world to the boundary a resume asks for and brings the
// fixtures it carried back up.
func (e *engine) restoreTo(target int, mode resumeMode) (int, bool) {
	ref, err := e.home.navigate(e.g.corridor(), target, mode)
	if err != nil {
		runFault(e.run, "%v", err)
		return 0, false
	}
	s, err := loadSession(e.home.sessionPath())
	if err != nil {
		runFault(e.run, "%v", err)
		return 0, false
	}
	e.run.SessionId = s.Id

	// everything the restored boundary covers was not executed by this invocation,
	// and the model says so rather than implying this run stands behind it.
	for _, cr := range e.run.Challenges[:ref.Boundary] {
		cr.Status = StatusRestored
	}
	// the deposits, world environment, and fixtures the restored world carries
	// replace whatever this process happened to be holding.
	if err := e.w.reload(); err != nil {
		runFault(e.run, "%v", err)
		return 0, false
	}
	// the restart belongs to the challenge about to run, here as much as at any
	// other boundary: if a fixture will not come back up, the finding is about the
	// challenge that never got it rather than about the bootstrap that preceded it.
	e.w.focus(e.run.Challenges[ref.Boundary])
	// a resumed run reaches its first challenge through the same bounce a full run
	// does, so the two cannot disagree about the world a challenge starts in.
	if !e.guard(func() { e.w.Restart() }) {
		e.run.Challenges[ref.Boundary].Status = StatusNotRun
		e.pendingReport = e.run.Challenges[ref.Boundary]
		return 0, false
	}
	return ref.Boundary, true
}

// runOne executes one challenge and closes its boundary, and reports whether the
// corridor continues.
func (e *engine) runOne(i int) bool {
	c := e.g.Challenges[i-1]
	cr := e.run.Challenges[i-1]
	e.w.focus(cr)
	cr.Status, cr.Started = StatusExecuted, time.Now()
	dl.Debugf("challenge %d/%d: %s", i, len(e.g.Challenges), c.Name)

	body := e.guard(func() {
		// the implicit expectations resolve on the way out too, so a command nobody
		// asserted is still accounted for when a terminal finding ends the challenge.
		defer e.w.resolvePending()
		c.Fn(e.w)
	})
	cr.Ended = time.Now()
	if !body {
		e.pendingReport = cr
		return false
	}

	// the boundary belongs to the challenge that just ran: a fixture found dead
	// here died in its window, and a snapshot taken here is its closed world.
	if !e.guard(func() {
		e.w.Quiesce()
		if c.NoCheckpoint {
			return
		}
		ref, err := e.w.publishBoundary(i, c.Name)
		if err != nil {
			e.w.faultf("%v", err)
			return
		}
		cr.Checkpoint = filepath.Base(ref.Dir)
	}) {
		// reported after cleanup, so a crash found while quiescing, a fault raised
		// while publishing, and a break still waiting on a fixture's fate all reach
		// the console rather than only the counts.
		e.pendingReport = cr
		e.writeTranscript()
		return false
	}

	if i == len(e.g.Challenges) {
		// nothing further will execute, so nothing is restarted — and nothing has
		// moved focus off this record yet, so cleanup can still add to it.
		e.pendingReport = cr
		e.writeTranscript()
		return true
	}

	// focus is about to move on, which is what settles the record: nothing left can
	// add to it, so it can be rendered.
	cr.seal()
	e.report(cr)
	e.writeTranscript()

	// the restart belongs to the challenge about to run, not the one that just
	// finished. its boundary is already published and truthful — a closed world
	// after a completed challenge — so the checkpoint stays and the finding
	// attributes forward, which is exactly the point a resume should return to.
	next := e.run.Challenges[i]
	e.w.focus(next)
	if !e.guard(func() { e.w.Restart() }) {
		next.Status = StatusNotRun
		// the finding belongs to the challenge that never got its fixture, and it is
		// the only account of why the corridor stopped here.
		e.pendingReport = next
		return false
	}
	return true
}

// finish ends the invocation: processes stopped, deferred findings settled, the
// corridor's untouched remainder recorded, and the narrative written.
func (e *engine) finish() {
	if r := recover(); r != nil {
		e.recoverPanic(r)
	}
	e.w.shutdown()
	for _, cr := range e.run.Challenges {
		if cr.Status == StatusPending {
			cr.Status = StatusNotRun
		}
	}
	// everything that could add to a record has now run, so every record settles
	// together and nothing the renderers read can change underneath them.
	if e.run.Bootstrap != nil {
		e.run.Bootstrap.seal()
	}
	for _, cr := range e.run.Challenges {
		cr.seal()
	}
	// the challenge that ended the run is reported now that cleanup has settled
	// whatever it was still waiting on.
	if e.pendingReport != nil {
		e.report(e.pendingReport)
		e.pendingReport = nil
	}
	e.writeTranscript()
}

// writeTranscript records the narrative, and ends the invocation if it cannot.
//
// this is the document you read when the guardian suite goes red, so a harness
// that cannot produce it has failed at something it promised — and a harness fault
// is terminal. cleanup still runs; it simply does not start another unwind.
func (e *engine) writeTranscript() {
	if e.transcript == nil {
		return
	}
	if err := e.transcript.write(e.run); err != nil {
		runFault(e.run, "%v", err)
		if !e.w.unwinding {
			panic(unwind{class: ClassFault})
		}
	}
}

// guard runs a phase of the invocation and reports whether it completed.
//
// a terminal finding leaves its challenge by panicking a private sentinel, which
// stops here. so does anything else: a suite that panics is a broken suite, and
// letting that escape would take the harness down with the run unaccounted for,
// the lock released, and whatever it started still running. it belongs in the
// census like any other way of being broken.
func (e *engine) guard(fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			e.recoverPanic(r)
			ok = false
		}
	}()
	fn()
	return true
}

// recoverPanic turns whatever ended a phase into something the census can carry.
func (e *engine) recoverPanic(r any) {
	if _, isUnwind := r.(unwind); isUnwind {
		return
	}
	if v, ok := r.(sealViolation); ok {
		// the record cannot carry this: it is the record that was written to too
		// late. the run says it instead.
		runFault(e.run, "the engine added to %q after sealing it, so something was reported before it was true", v.record)
		return
	}
	e.w.recordQuiet(ClassFault, fmt.Sprintf("the suite panicked: %v", r), string(debug.Stack()))
}

// report hands a settled challenge to the console reporter, if there is one.
//
// a renderer walks sealed records only, and the engine is what makes that true
// rather than what remembers it.
func (e *engine) report(cr *ChallengeRun) {
	if !cr.Sealed() {
		panic(sealViolation{record: cr.Name})
	}
	if e.opts.Report != nil {
		e.opts.Report.challenge(cr, e.opts.Verbose)
	}
}

// transcriptPath is where this invocation writes its narrative.
func (e *engine) transcriptPath() string {
	if e.opts.Transcript != "" {
		return e.opts.Transcript
	}
	return e.home.transcriptPath()
}

// interrupt records that the harness itself is ending the run and cancels the work
// in flight. provenance is recorded before any child is signalled, so a death that
// follows is the harness's own doing and never crash evidence.
func (e *engine) interrupt() {
	e.w.cancelled.Store(true)
}

// runFault records a harness fault that belongs to no challenge: a refused lock, a
// gauntlet the engine cannot run, a navigation that will not resolve.
func runFault(run *Run, format string, args ...any) {
	run.Findings = append(run.Findings, &Finding{
		Class: ClassFault, Message: fmt.Sprintf(format, args...), Step: -1, At: time.Now(),
	})
}
