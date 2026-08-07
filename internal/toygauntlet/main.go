// Command toygauntlet is a consumer of the challenge library, shaped the way a
// real one is: a package main that builds a gauntlet and hands it to
// challenge.Main.
//
// it exists so the engine and the standalone face are proven through the surface
// a consumer actually uses — real flags, real exit codes, real signals — rather
// than through an in-process approximation of them. what its challenges do is
// steered by environment variables, so one binary can be driven into every shape
// the engine claims to handle.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/michaelquigley/challenge"
)

// misbehaviour names how a challenge should fail, read from the environment as
// "<challenge>:<how>".
type misbehaviour struct {
	challenge string
	how       string
}

func main() {
	home := os.Getenv("TOYG_WORLD_HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "toygauntlet: TOYG_WORLD_HOME names where the world lives")
		os.Exit(2)
	}
	challenge.Main(challenge.Gauntlet{
		Name:       "toyg",
		WorldHome:  home,
		Bootstrap:  bootstrap,
		Challenges: corridor(),
	})
}

// bootstrap stands in for a consumer's build step: it puts the binary under test
// beside the world, where no checkpoint will ever contain it.
func bootstrap(w *challenge.W) error {
	if os.Getenv("TOYG_BOOTSTRAP_FAIL") != "" {
		return fmt.Errorf("the bootstrap was told to fail")
	}
	source := os.Getenv("TOYG_TOY")
	if source == "" {
		return fmt.Errorf("TOYG_TOY names the binary under test")
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(w.BinDir()+"/toy", body, 0o755)
}

// corridor is the gauntlet's shape, which the environment can rearrange so
// navigation can be pressed on a corridor that moved.
func corridor() []challenge.Challenge {
	all := []challenge.Challenge{
		{Name: "estate", Doc: "initializes the estate and deposits the digest a later challenge needs.", Fn: estate},
		{Name: "containers", Doc: "appends to the ledger, so running it twice is visible in the world.", Fn: containers},
		{Name: "slices", Doc: "collects the deposit an earlier challenge left.", Fn: slices},
		{Name: "durability", Doc: "stands up the server fixture and interrogates it over the wire.", Fn: durability},
		{Name: "restore", Doc: "compares the world against what the estate wrote.", Fn: restore},
	}
	for _, name := range strings.Split(os.Getenv("TOYG_NO_CHECKPOINT"), ",") {
		for i := range all {
			if all[i].Name == name {
				all[i].NoCheckpoint = true
			}
		}
	}
	switch {
	case os.Getenv("TOYG_REORDER") != "":
		// a challenge moved *before* the target: the old boundaries describe a
		// narrative that no longer exists.
		all[0], all[1] = all[1], all[0]
	case os.Getenv("TOYG_SUFFIX") != "":
		// the past intact and the future changed: a legitimate branch.
		all = append(all[:3:3], challenge.Challenge{Name: "recovery", Doc: "the branched future.", Fn: containers})
	}
	return all
}

func estate(w *challenge.W) {
	w.Run("toy state estate.yaml est-1 personal").ExpectMsg("wrote state")
	w.Put("digest", w.Run("toy digest").Capture(`snapshot ([0-9a-f]{12})`))
	misbehave(w, "estate")
}

func containers(w *challenge.W) {
	// appending rather than writing makes a second execution visible in the world,
	// which is how resume can be shown to have run against the right boundary.
	ledger := "ledger"
	body := ""
	if w.Has(ledger) {
		body = string(w.ReadFile(ledger))
	}
	w.WriteFile(ledger, []byte(body+"containers\n"))
	misbehave(w, "containers")
}

func slices(w *challenge.W) {
	w.Note("the digest was deposited by estate, and survives a resume because it rides the checkpoint")
	if got := w.Get("digest"); got != "9f2c1ab77e40" {
		w.Fail("the deposit came back as %q", got)
	}
	misbehave(w, "slices")
}

func durability(w *challenge.W) {
	port := w.FreePort()
	w.Start(challenge.Fixture{
		Name:         "server",
		Literal:      "toy serve --port {} --refuse-if {}",
		BaseURL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		ReadyURL:     "/api/v1/config",
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  10 * time.Second,
	}, port, w.Path("block-restart"))
	w.On("server").Get("/api/v1/state").ExpectStatus(200).ExpectBody(`"label"`)
	misbehave(w, "durability")
}

func restore(w *challenge.W) {
	w.Exists("estate.yaml")
	w.On("server").Get("/api/v1/config").ExpectStatus(200)
	misbehave(w, "restore")
}

// misbehave applies whatever failure the environment asked this challenge for.
func misbehave(w *challenge.W, name string) {
	for _, raw := range strings.Split(os.Getenv("TOYG_MISBEHAVE"), ",") {
		spec, ok := parse(raw)
		if !ok || spec.challenge != name {
			continue
		}
		switch spec.how {
		case "assert":
			w.Fail("this challenge was told to fail an assertion")
		case "break":
			w.Run("toy emit nothing to capture here").Capture(`snapshot ([0-9a-f]{12})`)
		case "crash":
			w.Run("toy panic")
		case "fault":
			w.Run("toy digest").Capture(`snapshot ([0-9a-f`)
		case "hang":
			w.Run("toy sleep 60s")
		case "quiet-exit":
			w.Run("toy fail this exit is nobody's business")
		case "suite-panic":
			// the suite itself coming apart, which is a different thing from the
			// product doing so.
			var nothing *string
			_ = *nothing
		case "restart-fail":
			// the fixture is fine now and will refuse to come back up at the
			// boundary, which is where the restart rule has to be exact.
			w.WriteFile("block-restart", []byte("no\n"))
		default:
			w.Fail("unknown misbehaviour %q", spec.how)
		}
	}
}

// parse reads a "<challenge>:<how>" instruction.
func parse(raw string) (misbehaviour, bool) {
	name, how, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok {
		return misbehaviour{}, false
	}
	return misbehaviour{challenge: name, how: how}, true
}
