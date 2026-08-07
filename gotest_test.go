package challenge

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inProcessGauntlet builds a small gauntlet against a world of its own, for
// exercising the faces without a consumer binary in the way.
func inProcessGauntlet(t *testing.T, challenges ...Challenge) Gauntlet {
	t.Helper()
	base := t.TempDir()
	t.Cleanup(func() { openTree(base) })
	toy := toyBinary(t)
	return Gauntlet{
		Name:      "faceg",
		WorldHome: base,
		Bootstrap: func(w *W) error {
			body, err := os.ReadFile(toy)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(w.BinDir(), "toy"), body, 0o755)
		},
		Challenges: challenges,
	}
}

func TestTheGoTestFaceRunsTheGauntletWhole(t *testing.T) {
	ran := map[string]bool{}
	g := inProcessGauntlet(t,
		Challenge{Name: "estate", Fn: func(w *W) {
			ran["estate"] = true
			w.Run("toy state estate.yaml est-1 personal").ExpectMsg("wrote state")
		}},
		Challenge{Name: "slices", Fn: func(w *W) {
			ran["slices"] = true
			w.Exists("estate.yaml")
		}},
	)

	// the face is a walker: each challenge becomes a subtest reporting what the
	// engine recorded against it, and the corridor runs whole because navigation
	// belongs to the standalone face alone.
	T(t, g)
	assert.True(t, ran["estate"])
	assert.True(t, ran["slices"])
}

// TestGoTestFaceFixture is not a test of anything on its own. it is the body the
// pin below runs as a subprocess, so a face that is supposed to fail can be watched
// failing without failing the run watching it — and watched in its real habitat,
// under go test, rather than in an approximation of one.
func TestGoTestFaceFixture(t *testing.T) {
	if os.Getenv("CHALLENGE_FACE_FIXTURE") == "" {
		t.Skip("driven as a subprocess by TestTheGoTestFaceReportsWhatTheEngineRecorded")
	}
	T(t, inProcessGauntlet(t,
		Challenge{Name: "estate", Fn: func(w *W) { w.Fail("a wording mismatch") }},
		Challenge{Name: "slices", Fn: func(w *W) { w.Exists("never-written") }},
	))
}

func TestTheGoTestFaceReportsWhatTheEngineRecorded(t *testing.T) {
	cmd := exec.Command("go", "test", "-count=1", "-run", "TestGoTestFaceFixture", "-v", ".")
	cmd.Env = append(os.Environ(), "CHALLENGE_FACE_FIXTURE=1")
	out, err := cmd.CombinedOutput()

	require.Error(t, err, "a finding reaches the go-test face as a failure")
	text := string(out)

	// each challenge is a subtest reporting what the engine recorded against it, so
	// a failure names the challenge it belongs to rather than the whole run.
	assert.Contains(t, text, "--- FAIL: TestGoTestFaceFixture/estate")
	assert.Contains(t, text, "--- FAIL: TestGoTestFaceFixture/slices")
	assert.Contains(t, text, "assertion: a wording mismatch")
	assert.Contains(t, text, "expected never-written to exist")
}

func TestTheStandaloneFaceCarriesTheVerdict(t *testing.T) {
	g := inProcessGauntlet(t,
		Challenge{Name: "estate", Fn: func(w *W) { w.Run("toy emit fine") }},
	)

	var out, errOut nopWriter
	assert.Equal(t, 0, MainWith(t.Context(), g, nil, &out, &errOut))

	// the corridor it lists is the corridor it would run.
	assert.Equal(t, 0, MainWith(t.Context(), g, []string{"--list"}, &out, &errOut))

	// an unknown challenge is a broken harness input, not a product finding.
	assert.Equal(t, 2, MainWith(t.Context(), g, []string{"--from", "nonesuch"}, &out, &errOut))

	// and asking for two different runs at once is refused rather than guessed at.
	assert.Equal(t, 2, MainWith(t.Context(), g, []string{"--from", "estate", "--only", "estate"}, &out, &errOut))

	// an argument the face does not know is refused before anything is touched.
	assert.Equal(t, 2, MainWith(t.Context(), g, []string{"--nonesuch"}, &out, &errOut))
	assert.Equal(t, 2, MainWith(t.Context(), g, []string{"stray"}, &out, &errOut))
}

func TestAGauntletTheEngineCannotRunIsAFault(t *testing.T) {
	body := func(w *W) {}
	for name, g := range map[string]Gauntlet{
		"no challenges": {Name: "faceg", WorldHome: t.TempDir()},
		"unnamed challenge": {Name: "faceg", WorldHome: t.TempDir(),
			Challenges: []Challenge{{Fn: body}}},
		"duplicate names": {Name: "faceg", WorldHome: t.TempDir(),
			Challenges: []Challenge{{Name: "estate", Fn: body}, {Name: "estate", Fn: body}}},
		"bodyless challenge": {Name: "faceg", WorldHome: t.TempDir(),
			Challenges: []Challenge{{Name: "estate"}}},
		"relative home": {Name: "faceg", WorldHome: "relative",
			Challenges: []Challenge{{Name: "estate", Fn: body}}},
		"unusable name": {Name: "..", WorldHome: t.TempDir(),
			Challenges: []Challenge{{Name: "estate", Fn: body}}},
	} {
		run := Execute(t.Context(), g, Options{})
		assert.Equal(t, 2, run.Verdict(), name)
		require.NotEmpty(t, run.Findings, name)
		assert.Equal(t, ClassFault, run.Findings[0].Class, name)
	}
}

// nopWriter discards what a face renders, for the pins that care only about the
// verdict it returned.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
