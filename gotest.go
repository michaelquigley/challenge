package challenge

import (
	"bytes"
	"context"
	"testing"
)

// T is the go test face: the same run, mapped onto testing.T for IDE integration
// and unified CI reporting.
//
// it lives behind a build tag in the consumer, so `go test ./...` never trips a
// system suite by accident — unit tests and system tests do not mix by mechanism
// rather than by convention. and it runs the gauntlet whole: navigation belongs to
// the standalone face alone and stays there.
//
// the invocation that means anything is `go test -count=1 -tags <tag> ...`. the
// import seam keeps the product's source out of this package's cache key, so
// without -count=1 the toolchain can answer "ok (cached)" for a product it never
// rebuilt and never ran. a cached verdict is no verdict.
//
// like every other face this is a walker: each challenge becomes a subtest that
// reports the findings the engine recorded against it, and nothing here decides
// what a finding means.
func T(t *testing.T, g Gauntlet) {
	t.Helper()

	var report bytes.Buffer
	run := Execute(context.Background(), g, Options{Report: newConsoleReporter(&report)})

	for _, f := range run.Findings {
		t.Errorf("%s: %s", f.Class, f.Message)
	}
	if run.Bootstrap != nil {
		for _, f := range run.Bootstrap.Findings {
			t.Errorf("bootstrap %s: %s", f.Class, f.Message)
		}
	}

	for _, cr := range run.Challenges {
		t.Run(cr.Name, func(t *testing.T) {
			// findings come first, whatever the status. a restart failure is
			// deliberately attributed to the challenge that never got its fixture,
			// and skipping past it would hide the only account of why the corridor
			// stopped there.
			for _, f := range cr.Findings {
				at := ""
				if f.Step >= 0 && f.Step < len(cr.Steps) {
					at = "\n  at: " + cr.Steps[f.Step].Label
				}
				t.Errorf("%s: %s%s", f.Class, f.Message, at)
			}
			if len(cr.Findings) > 0 {
				return
			}
			switch cr.Status {
			case StatusRestored:
				t.Skip("restored from a checkpoint rather than executed")
			case StatusSkipped:
				t.Skip("not requested")
			case StatusNotRun:
				t.Skip("not reached: the invocation ended first")
			}
		})
	}

	if run.Verdict() != 0 && !t.Failed() {
		// a verdict the subtests did not account for still has to arrive somewhere.
		t.Errorf("the run exited %d\n%s", run.Verdict(), report.String())
	}
}
