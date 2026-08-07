package challenge

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/michaelquigley/df/dd"
)

// crashMarkers are the textual evidence of a Go process coming apart. this is the
// same heuristic the bash suite has always used; a product that legitimately
// prints one of these would need an escape hatch, and none exists until one does.
var crashMarkers = []string{"panic:", "goroutine "}

// verdictPending is a completed result whose status nobody asserted. the engine
// resolves them when a challenge ends, so an unasserted failure is loud rather
// than silent.
type verdictPending interface {
	resolveVerdict()
}

// dlLine is the shape of one dl message on the wire, mirrored narrowly: the
// harness parses what comes back and cares about one field of it.
type dlLine struct {
	Msg string
}

// Result is one command invocation's outcome: the exit code, the raw streams as
// the shell would have shown them, and the dl messages parsed out as data.
type Result struct {
	Literal string
	Exit    int
	Stdout  string
	Stderr  string
	Msgs    []string

	w            *W
	step         int
	exitAsserted bool
}

// resolveVerdict applies the implicit expectation an unasserted result carries.
//
// the verdict is total: a command nobody asked about is still expected to have
// succeeded. this is the loud abort the bash could never give its silenced setup
// lines, where a failing step disappeared into /dev/null and surfaced later as a
// confusing downstream failure.
func (r *Result) resolveVerdict() {
	if r.exitAsserted || r.Exit == 0 {
		return
	}
	r.w.recordAt(ClassAssertion, r.step,
		fmt.Sprintf("%q exited %d with nothing expecting it to", r.Literal, r.Exit), r.combined())
}

// combined is the raw surface the shell would have shown, stdout then stderr.
//
// the streams are joined by a newline rather than butted together, so nothing can
// be matched across the seam between them — a fragment ending one stream and a
// fragment beginning the other are not a thing that ever appeared anywhere.
func (r *Result) combined() string {
	return r.Stdout + "\n" + r.Stderr
}

// ExpectExit asserts the wire status. naming one displaces the implicit
// expectation that the command succeeded.
func (r *Result) ExpectExit(code int) *Result {
	r.exitAsserted = true
	if r.Exit != code {
		r.w.recordAt(ClassAssertion, r.step,
			fmt.Sprintf("%q exited %d, expected %d", r.Literal, r.Exit, code), r.combined())
	}
	return r
}

// ExpectMsg asserts a parsed dl message contains a substring. wording is contract:
// this is the surface the user actually experiences.
func (r *Result) ExpectMsg(substr string) *Result {
	for _, m := range r.Msgs {
		if strings.Contains(m, substr) {
			return r
		}
	}
	r.w.recordAt(ClassAssertion, r.step,
		fmt.Sprintf("%q emitted no message containing %q", r.Literal, substr), strings.Join(r.Msgs, "\n"))
	return r
}

// ExpectNoMsg asserts no parsed message contains a substring.
func (r *Result) ExpectNoMsg(substr string) *Result {
	for _, m := range r.Msgs {
		if strings.Contains(m, substr) {
			r.w.recordAt(ClassAssertion, r.step,
				fmt.Sprintf("%q emitted a message containing %q", r.Literal, substr), m)
			return r
		}
	}
	return r
}

// ExpectMsgOnce asserts a substring appears exactly once across the raw streams.
//
// this is the transport-discipline check: an operational failure should render
// once, and a message that arrives twice means it travelled two paths to the
// terminal. counting the raw streams rather than the parsed messages is what
// catches the duplicate that parsing would deduplicate away.
func (r *Result) ExpectMsgOnce(substr string) *Result {
	n := strings.Count(r.combined(), substr)
	if n != 1 {
		r.w.recordAt(ClassAssertion, r.step,
			fmt.Sprintf("%q rendered %q %d times, expected exactly once", r.Literal, substr, n), r.combined())
	}
	return r
}

// ExpectOut asserts a substring of raw stdout. tables rendered by go-pretty are
// not dl messages and carry variable cell padding, so assertions against them
// target single cells or header words, never phrases spanning columns.
func (r *Result) ExpectOut(substr string) *Result {
	return r.expectRaw("stdout", r.Stdout, substr, true)
}

// ExpectNoOut asserts a substring is absent from raw stdout.
func (r *Result) ExpectNoOut(substr string) *Result {
	return r.expectRaw("stdout", r.Stdout, substr, false)
}

// ExpectErr asserts a substring of raw stderr.
func (r *Result) ExpectErr(substr string) *Result {
	return r.expectRaw("stderr", r.Stderr, substr, true)
}

// ExpectNoErr asserts a substring is absent from raw stderr.
func (r *Result) ExpectNoErr(substr string) *Result {
	return r.expectRaw("stderr", r.Stderr, substr, false)
}

func (r *Result) expectRaw(stream, body, substr string, want bool) *Result {
	if strings.Contains(body, substr) != want {
		verb := "did not contain"
		if !want {
			verb = "contained"
		}
		r.w.recordAt(ClassAssertion, r.step,
			fmt.Sprintf("%q %s %s %q", r.Literal, stream, verb, substr), body)
	}
	return r
}

// Capture extracts a value from the raw streams for the rest of the challenge to
// use. it reads stdout then stderr — the same surface the crash scan reads, and
// the one where a digest appears literally inside the dl line that carries it.
//
// exactly one usable match is required. an invalid expression is a broken harness
// input and faults; zero or ambiguous matches are a break — the output did not say
// what the suite claims it says — and a break is terminal, so the failure surfaces
// where it happened rather than ripening into a missing deposit two challenges
// later, wearing the wrong class.
//
// the first capture group is returned when the pattern has one, the whole match
// otherwise.
func (r *Result) Capture(pattern string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		r.w.faultf("capture expression %q does not compile: %v", pattern, err)
	}
	matches := re.FindAllStringSubmatch(r.combined(), -1)
	switch {
	case len(matches) == 0:
		r.w.recordAt(ClassBreak, r.step,
			fmt.Sprintf("%q produced nothing matching %q", r.Literal, pattern), r.combined())
		// recording returns rather than unwinding once the run is already on its
		// way out. there is no value to hand back at that point, and nothing left
		// that would use one.
		return ""
	case len(matches) > 1:
		r.w.recordAt(ClassBreak, r.step,
			fmt.Sprintf("%q produced %d matches for %q, expected exactly one", r.Literal, len(matches), pattern), r.combined())
		return ""
	}
	if len(matches[0]) > 1 {
		return matches[0][1]
	}
	return matches[0][0]
}

// detectCrash flags a command that came apart, without an explicit check per
// invocation.
//
// detection is evidential as well as textual: a process terminated by a signal may
// emit no marker at all. provenance is respected, and it is provenance for *this*
// command — a kill the harness delivered to this process, not merely a run that was
// interrupted around it. the crash tier is only worth having if it cannot be
// triggered by the harness's own hand, and equally it must not be erased by a
// cancellation that did not cause the death. marker and exit evidence for one
// death coalesce into one finding.
func (r *Result) detectCrash(state *os.ProcessState, killedByHarness bool) {
	var evidence []string
	// each stream is read on its own. a marker is evidence only where it actually
	// appeared, and the highest-order finding in the census is not one to invent
	// out of two halves that were never adjacent.
	for stream, body := range map[string]string{"stdout": r.Stdout, "stderr": r.Stderr} {
		if marker := findMarker(body); marker != "" {
			evidence = append(evidence, fmt.Sprintf("%s carries %q", stream, marker))
		}
	}
	sort.Strings(evidence)
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() && !killedByHarness {
		evidence = append(evidence, fmt.Sprintf("terminated by signal %s", status.Signal()))
	}
	if len(evidence) == 0 {
		return
	}
	r.w.recordAt(ClassCrash, r.step,
		fmt.Sprintf("%q crashed: %s", r.Literal, strings.Join(evidence, ", ")), r.combined())
}

// findMarker reports the first crash marker present in a body, or empty.
func findMarker(body string) string {
	for _, m := range crashMarkers {
		if strings.Contains(body, m) {
			return m
		}
	}
	return ""
}

// parseMsgs pulls dl messages out of a stream. a harness subprocess is always
// piped, so dl selects its JSON transport and the messages arrive as data; the
// wording is identical to what a terminal would have shown.
func parseMsgs(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var parsed dlLine
		if err := dd.BindJSON(&parsed, []byte(line)); err != nil {
			continue
		}
		if parsed.Msg != "" {
			out = append(out, parsed.Msg)
		}
	}
	return out
}

// resolvePending applies the implicit expectations of every result nobody asserted
// and clears the list. the engine calls it when a challenge ends, including on the
// way out of a terminal finding.
func (w *W) resolvePending() {
	pending := w.pending
	w.pending = nil
	for _, p := range pending {
		p.resolveVerdict()
	}
}
