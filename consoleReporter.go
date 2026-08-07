package challenge

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// consoleReporter walks the run model to a terminal as the run happens.
//
// it holds no logic of its own: what a finding means, what the verdict is, and
// which class anything belongs to are all read off the model. a renderer that
// computed any of that would be a second source of truth able to drift from the
// one the engine wrote.
type consoleReporter struct {
	out io.Writer
}

// newConsoleReporter reports to a writer.
func newConsoleReporter(out io.Writer) *consoleReporter {
	return &consoleReporter{out: out}
}

// challenge reports one completed challenge. verbose asks for every step rather
// than for the findings alone.
func (r *consoleReporter) challenge(cr *ChallengeRun, verbose bool) {
	fmt.Fprintf(r.out, "%s %s (%s)\n", challengeMark(cr), cr.Name, cr.Ended.Sub(cr.Started).Round(time.Millisecond))
	if verbose {
		for _, s := range cr.Steps {
			fmt.Fprintf(r.out, "    %-5s %s\n", s.Kind, s.Label)
		}
	}
	for _, f := range cr.Findings {
		r.finding(f, cr)
	}
}

// finding reports one finding and the step it belongs to.
func (r *consoleReporter) finding(f *Finding, cr *ChallengeRun) {
	fmt.Fprintf(r.out, "    %s: %s\n", f.Class, f.Message)
	if f.Step >= 0 && f.Step < len(cr.Steps) {
		fmt.Fprintf(r.out, "      at: %s\n", cr.Steps[f.Step].Label)
	}
	for _, line := range detailLines(f.Detail, 8) {
		fmt.Fprintf(r.out, "      %s\n", line)
	}
}

// summary reports what the whole invocation came to.
func (r *consoleReporter) summary(run *Run) {
	for _, f := range run.Findings {
		fmt.Fprintf(r.out, "%s: %s\n", f.Class, f.Message)
	}
	if run.Bootstrap != nil {
		for _, f := range run.Bootstrap.Findings {
			fmt.Fprintf(r.out, "bootstrap %s: %s\n", f.Class, f.Message)
		}
	}

	executed, restored, notRun := 0, 0, 0
	for _, cr := range run.Challenges {
		switch cr.Status {
		case StatusExecuted:
			executed++
		case StatusRestored:
			restored++
		case StatusNotRun:
			notRun++
		}
	}

	fmt.Fprintf(r.out, "\n%s %s run %s: %d executed", verdictMark(run), run.Gauntlet, run.RunId, executed)
	if restored > 0 {
		fmt.Fprintf(r.out, ", %d restored", restored)
	}
	if notRun > 0 {
		fmt.Fprintf(r.out, ", %d not run", notRun)
	}
	for _, class := range []FindingClass{ClassFault, ClassCrash, ClassBreak, ClassAssertion} {
		if n := run.Count(class); n > 0 {
			fmt.Fprintf(r.out, ", %d %s", n, class)
		}
	}
	if run.Interrupted {
		fmt.Fprint(r.out, ", interrupted")
	}
	fmt.Fprintf(r.out, " (exit %d)\n", run.Verdict())
}

// list reports a gauntlet's corridor without running it.
func (r *consoleReporter) list(g Gauntlet) {
	for i, c := range g.Challenges {
		policy := ""
		if c.NoCheckpoint {
			policy = "  (no checkpoint)"
		}
		fmt.Fprintf(r.out, "%2d  %s%s\n", i+1, c.Name, policy)
	}
}

// challengeMark is the short shape of a challenge's outcome, asked of the model
// rather than derived here.
func challengeMark(cr *ChallengeRun) string {
	worst, any := cr.Worst()
	if !any {
		return "ok  "
	}
	return fmt.Sprintf("%-4s", worst.String()[:4])
}

// verdictMark names the wire status the model computed.
func verdictMark(run *Run) string {
	switch run.Verdict() {
	case 0:
		return "clean"
	case 1:
		return "findings"
	default:
		return "invalid"
	}
}

// detailLines keeps a finding's evidence readable: the tail of it, indented, and
// bounded so one enormous body cannot bury the rest of the report.
func detailLines(detail string, max int) []string {
	detail = strings.TrimRight(detail, "\n")
	if detail == "" {
		return nil
	}
	lines := strings.Split(detail, "\n")
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return lines
}
