package challenge

import (
	"fmt"
	"time"
)

// FindingClass is the error census. every failure the harness can observe belongs
// to exactly one class, and the class governs both the verdict and the control
// flow that follows it. a guardian suite's worst failure is a verdict that lies —
// a harness bug reading as a product failure, a product regression reading as a
// broken harness, or a silent pass — so the classes are never blurred.
type FindingClass int

const (
	// ClassAssertion is a counted finding: a wording or value mismatch that severs
	// no dependent flow. the corridor continues through it and the run exits 1.
	// this is the only non-terminal class.
	ClassAssertion FindingClass = iota

	// ClassBreak is a product-surface failure that severs dependent flow: a failed
	// capture, a decode mismatch, a refused wire on a well-formed request, a
	// fixture that never became ready. terminal for the invocation, exit 1, ranked
	// below crash.
	ClassBreak

	// ClassCrash is the highest-order product finding: a panic marker, an
	// unsolicited death by signal, a supervised process found dead before its
	// requested quiesce. terminal for the invocation, exit 1.
	ClassCrash

	// ClassFault is a harness fault: the harness or the suite itself is broken —
	// an invalid input, unreadable harness-owned state, a spawn or setup failure, a
	// quiesce or cleanup failure. the run is invalid and exits 2.
	ClassFault
)

// String names the class the way every renderer must name it.
func (c FindingClass) String() string {
	switch c {
	case ClassAssertion:
		return "assertion"
	case ClassBreak:
		return "break"
	case ClassCrash:
		return "crash"
	case ClassFault:
		return "fault"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// Terminal reports whether a finding of this class ends the invocation. every
// class but assertion is terminal: a broken data flow must never send a zero value
// onward to fail somewhere confusing.
func (c FindingClass) Terminal() bool {
	return c != ClassAssertion
}

// Finding is one recorded failure, carrying the class that governs its verdict.
type Finding struct {
	Class   FindingClass
	Message string
	Detail  string
	// Step indexes the owning challenge's steps, or -1 when the finding is not
	// scoped to a step.
	Step int
	At   time.Time
}

// StepKind names the channels a challenge acts through.
type StepKind int

const (
	// StepCmd is a subprocess invocation of the system's shipped command surface.
	StepCmd StepKind = iota
	// StepHttp is a request against the system's shipped wire surface.
	StepHttp
	// StepFs is an observation of, or a deliberate mutation to, the world's files.
	StepFs
	// StepNote is authored prose recorded into the narrative.
	StepNote
)

// String names the step kind for renderers.
func (k StepKind) String() string {
	switch k {
	case StepCmd:
		return "cmd"
	case StepHttp:
		return "http"
	case StepFs:
		return "fs"
	case StepNote:
		return "note"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// Step is one action a challenge took, recorded as it happened.
type Step struct {
	Kind StepKind
	// Label is what the step did in the register a reader recognizes: the command
	// literal, the request line, the path, the note.
	Label string
	// Detail carries the output worth keeping beside the label.
	Detail string
	// Exit is the command's exit code; StepCmd only.
	Exit int
	// Status is the response status; StepHttp only.
	Status  int
	At      time.Time
	Elapsed time.Duration
}

// ChallengeStatus records what an invocation did with a challenge. it is about
// this invocation only: a resumed run restores a prefix it did not execute, and
// says so.
type ChallengeStatus int

const (
	// StatusPending is a challenge the invocation has not reached.
	StatusPending ChallengeStatus = iota
	// StatusRestored is a challenge covered by the restored checkpoint rather than
	// executed by this invocation.
	StatusRestored
	// StatusExecuted is a challenge this invocation ran.
	StatusExecuted
	// StatusNotRun is a challenge the invocation never reached because a terminal
	// finding ended the corridor first.
	StatusNotRun
	// StatusSkipped is a challenge the invocation was never asked to run, which is
	// a different statement from one it failed to reach.
	StatusSkipped
)

// String names the status for renderers.
func (s ChallengeStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRestored:
		return "restored"
	case StatusExecuted:
		return "executed"
	case StatusNotRun:
		return "not-run"
	case StatusSkipped:
		return "not-requested"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

// ChallengeRun is one challenge's record within a run.
type ChallengeRun struct {
	Name     string
	Doc      string
	Status   ChallengeStatus
	Steps    []*Step
	Findings []*Finding
	// Checkpoint names the boundary checkpoint published after this challenge, or
	// is empty when none was — the challenge opted out, or a terminal finding made
	// its window ineligible.
	Checkpoint string
	Started    time.Time
	Ended      time.Time

	// sealed records that every phase which could add to this record has finished.
	sealed bool
}

// Sealed reports whether this record is settled — whether everything that could
// still add a step or a finding to it has run.
//
// renderers walk only sealed records. a challenge is not finished when its body
// returns: its boundary can still find a fixture dead, cleanup can still settle a
// wire failure that was waiting to learn what it was, and a report written before
// those would say "ok" beside a finding the model was about to record.
func (c *ChallengeRun) Sealed() bool { return c.sealed }

// seal declares a record settled. from here, adding to it is a programming error
// rather than a finding: it means the engine published something as true before it
// was.
func (c *ChallengeRun) seal() { c.sealed = true }

// sealViolation is panicked when something adds to a record already declared
// settled. it is not a class in the census — the census is about the world under
// test, and this is about the harness sequencing itself wrongly.
type sealViolation struct {
	record string
}

// Worst reports the highest-order class among a challenge's findings, and whether
// it recorded any. ranking classes is census knowledge and lives with the census,
// so a renderer asks rather than deriving an order of its own.
func (c *ChallengeRun) Worst() (FindingClass, bool) {
	if len(c.Findings) == 0 {
		return ClassAssertion, false
	}
	worst := c.Findings[0].Class
	for _, f := range c.Findings {
		if f.Class > worst {
			worst = f.Class
		}
	}
	return worst, true
}

// Run is the model of one invocation: pure data, written by the engine and walked
// by every renderer. verdicts are per-invocation and computed only from work this
// invocation executed.
type Run struct {
	Gauntlet string
	// SessionId identifies the world generation — minted at reset, carried by every
	// checkpoint the session publishes.
	SessionId string
	// RunId identifies this invocation, stamped through the model, the transcript's
	// attempt section, and the checkpoints it publishes.
	RunId   string
	Started time.Time
	Ended   time.Time
	// Bootstrap records what the consumer's pre-run hook did, kept apart from the
	// corridor so its steps and findings are attributable to it rather than landing
	// on whichever challenge happened to come first.
	Bootstrap  *ChallengeRun
	Challenges []*ChallengeRun
	// Findings holds run-scoped findings: the ones that belong to no challenge,
	// such as a refused lock, a failed bootstrap, or a divergent corridor.
	Findings []*Finding
	// Interrupted records that a signal ended the run. an interrupted run is an
	// invalid run.
	Interrupted bool
}

// Challenge returns the record for a named challenge, or nil.
func (r *Run) Challenge(name string) *ChallengeRun {
	for _, c := range r.Challenges {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// AllFindings walks every finding in the run: run-scoped first, then the
// bootstrap's, then each challenge's in corridor order.
func (r *Run) AllFindings() []*Finding {
	out := make([]*Finding, 0, len(r.Findings))
	out = append(out, r.Findings...)
	if r.Bootstrap != nil {
		out = append(out, r.Bootstrap.Findings...)
	}
	for _, c := range r.Challenges {
		out = append(out, c.Findings...)
	}
	return out
}

// Count returns how many findings of a class this run recorded.
func (r *Run) Count(class FindingClass) int {
	n := 0
	for _, f := range r.AllFindings() {
		if f.Class == class {
			n++
		}
	}
	return n
}

// Verdict is the wire status this invocation earned: 0 clean, 1 findings, 2
// harness fault. it is computed from the model alone, so every renderer reports
// the same verdict without deriving one of its own.
func (r *Run) Verdict() int {
	// an interrupted run is an invalid run whether or not it managed to record
	// anything on the way out. it earns the harness-fault tier from the fact of
	// the interruption, not from the findings that happened to land first.
	if r.Interrupted {
		return 2
	}
	fault, finding := false, false
	for _, f := range r.AllFindings() {
		if f.Class == ClassFault {
			fault = true
		} else {
			finding = true
		}
	}
	switch {
	case fault:
		return 2
	case finding:
		return 1
	default:
		return 0
	}
}
