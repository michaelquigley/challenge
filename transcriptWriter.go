package challenge

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// transcriptWriter renders the run model to a readable document as the run
// happens.
//
// the transcript is a projection of execution, generated from the model and never
// a second source that can drift. it is rewritten as each challenge completes, so
// a failed or aborted run leaves the document that explains it — the thing you
// read when the guardian suite goes red.
//
// attempts accumulate rather than overwrite. a resumed invocation opens a new
// section against its restored boundary instead of appending silently into the
// prior narrative, so the document stays honest about what ran when.
type transcriptWriter struct {
	path string
	// prior is everything earlier attempts wrote, held so this attempt can be
	// re-rendered in place without disturbing them.
	prior string
	// reported records that a failure has already been raised, so a transcript that
	// cannot be written complains once rather than at every boundary.
	reported bool
}

// newTranscriptWriter opens a transcript, keeping whatever earlier attempts left.
//
// no prior transcript is the ordinary case for a fresh world. one that exists and
// cannot be read is a different thing: the harness cannot say what earlier attempts
// recorded, and continuing would silently discard them.
func newTranscriptWriter(path string) (*transcriptWriter, error) {
	prior, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &transcriptWriter{path: path}, nil
		}
		return nil, fmt.Errorf("reading the transcript %s: %w", path, err)
	}
	return &transcriptWriter{path: path, prior: string(prior)}, nil
}

// discardPrior drops the attempts a discarded world generation left behind.
func (t *transcriptWriter) discardPrior() {
	t.prior = ""
}

// write renders this attempt beside the ones before it.
//
// a transcript that will not write is a harness fault, not a run that happened to
// produce no narrative. this is the document you read when the guardian suite goes
// red, and a green verdict beside a missing one would be the harness quietly
// failing to keep its own promise.
func (t *transcriptWriter) write(run *Run) error {
	if t.reported {
		return nil
	}
	if err := os.WriteFile(t.path, []byte(t.prior+renderTranscript(run)), 0o644); err != nil {
		t.reported = true
		return fmt.Errorf("writing the transcript %s: %w", t.path, err)
	}
	return nil
}

// renderTranscript walks a run model into markdown.
func renderTranscript(run *Run) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## attempt %s\n\n", run.RunId)
	fmt.Fprintf(&b, "- gauntlet: `%s`\n", run.Gauntlet)
	if run.SessionId != "" {
		fmt.Fprintf(&b, "- session: `%s`\n", run.SessionId)
	}
	fmt.Fprintf(&b, "- started: %s\n", run.Started.Format(time.RFC3339))
	fmt.Fprintf(&b, "- verdict: %s (exit %d)\n", verdictMark(run), run.Verdict())
	if run.Interrupted {
		fmt.Fprint(&b, "- interrupted\n")
	}
	b.WriteString("\n")

	for _, f := range run.Findings {
		fmt.Fprintf(&b, "**%s** — %s\n\n", f.Class, f.Message)
	}
	if run.Bootstrap != nil && len(run.Bootstrap.Findings) > 0 {
		b.WriteString("### bootstrap\n\n")
		for _, f := range run.Bootstrap.Findings {
			fmt.Fprintf(&b, "**%s** — %s\n\n", f.Class, f.Message)
		}
	}

	for _, cr := range run.Challenges {
		renderChallenge(&b, cr)
	}
	return b.String()
}

// renderChallenge walks one challenge's record.
//
// an unsealed record is one the engine has not finished writing, and rendering it
// would put something in the narrative that is not yet true. it appears once it
// settles.
func renderChallenge(b *strings.Builder, cr *ChallengeRun) {
	if !cr.Sealed() {
		return
	}
	fmt.Fprintf(b, "### %s — %s\n\n", cr.Name, cr.Status)
	if cr.Doc != "" {
		fmt.Fprintf(b, "%s\n\n", cr.Doc)
	}
	for _, s := range cr.Steps {
		renderStep(b, s)
	}
	if len(cr.Steps) > 0 {
		b.WriteString("\n")
	}
	// findings are rendered whatever the status. a challenge that never ran because
	// its fixture would not come back up carries the finding that explains why, and
	// hiding it would leave the not-run unaccounted for.
	for _, f := range cr.Findings {
		fmt.Fprintf(b, "**%s** — %s\n\n", f.Class, f.Message)
		if lines := detailLines(f.Detail, 20); len(lines) > 0 {
			fmt.Fprintf(b, "```\n%s\n```\n\n", strings.Join(lines, "\n"))
		}
	}
	if cr.Checkpoint != "" {
		fmt.Fprintf(b, "checkpoint: `%s`\n\n", cr.Checkpoint)
	}
}

// renderStep walks one action, in the register the reader recognizes.
func renderStep(b *strings.Builder, s *Step) {
	switch s.Kind {
	case StepCmd:
		fmt.Fprintf(b, "    $ %s", s.Label)
		if s.Exit != 0 {
			fmt.Fprintf(b, "   (exit %d)", s.Exit)
		}
		b.WriteString("\n")
	case StepHttp:
		fmt.Fprintf(b, "    > %s   (%d)\n", s.Label, s.Status)
	case StepNote:
		fmt.Fprintf(b, "    # %s\n", s.Label)
	default:
		fmt.Fprintf(b, "    . %s\n", s.Label)
	}
}
