package challenge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/michaelquigley/df/dd"
)

// kvName and envName are the harness-owned deposits inside the checkpoint image.
const (
	kvName  = "kv.yaml"
	envName = "env.yaml"
)

// kvFile is the on-disk shape of the world's deposit store. it lives under the
// reserved harness directory inside the world, so a value one challenge deposits
// for a later one rides the checkpoint with everything else and rolls back with a
// restore.
type kvFile struct {
	Values map[string]string
}

// envFile is the on-disk shape of the world-level environment applied to every
// invocation. it sits beside the deposits for the same reason.
type envFile struct {
	Values map[string]string
}

// unwind is the sentinel a terminal finding panics with. the engine recovers it.
//
// a challenge is ordinary Go, so the only way to stop one mid-body is to leave it
// — and leaving is exactly what a terminal finding means. the alternative, letting
// a failed capture or an undecodable body return a zero value, is how a break
// ripens into a confusing failure three steps downstream wearing the wrong class.
type unwind struct {
	class FindingClass
}

// W is the world handle: the singular living environment a gauntlet runs against,
// and the surface every challenge acts through.
type W struct {
	home *home
	run  *Run
	cur  *ChallengeRun
	kv   map[string]string
	env  map[string]string

	// ctx carries harness cancellation into every child the world spawns.
	ctx context.Context
	// cancelled records that the harness itself began terminating its children.
	// it is set before any child is signalled, so a death observed afterward is
	// the harness's own doing and never crash evidence.
	cancelled atomic.Bool
	// unwinding records that the run is already on its way out, so a finding
	// raised during cleanup is recorded rather than re-entering the unwind.
	unwinding bool

	// pending holds completed results whose status nobody asserted. they carry an
	// implicit expectation resolved when the challenge ends.
	pending []verdictPending
	// pendingBreaks holds wire failures whose meaning is not settled until the
	// fixture behind them has stopped.
	pendingBreaks []pendingBreak
	// crashedFixtures names every fixture whose collapse has been attributed, so a
	// deferred break knows not to report the same event a second time.
	crashedFixtures map[string]bool

	// specs is the registered fixture declarations, in start order. it rides the
	// checkpoint image, so a resumed run knows what to restart and a restore rolls
	// back fixtures a later challenge registered.
	specs []fixtureSpec
	// instances is the live processes, keyed by fixture name.
	instances map[string]*instance
}

// newW opens a world handle over an existing tree, loading the harness-owned state
// that rides the checkpoint image.
func newW(ctx context.Context, h *home, run *Run, cur *ChallengeRun) (*W, error) {
	w := &W{
		home:            h,
		run:             run,
		cur:             cur,
		ctx:             ctx,
		instances:       map[string]*instance{},
		crashedFixtures: map[string]bool{},
	}
	if err := w.reload(); err != nil {
		return nil, err
	}
	return w, nil
}

// reload re-reads the harness-owned world state. the engine calls it after a
// restore, so deposits, world environment, and registered fixtures roll back with
// the world rather than surviving in memory as facts from a future that was
// abandoned.
func (w *W) reload() error {
	kv, err := readHarnessMap(filepath.Join(w.home.harness(), kvName))
	if err != nil {
		return err
	}
	env, err := readHarnessMap(filepath.Join(w.home.harness(), envName))
	if err != nil {
		return err
	}
	specs, err := readProcessRegistry(filepath.Join(w.home.harness(), processName))
	if err != nil {
		return err
	}
	w.kv, w.env, w.specs = kv, env, specs
	return nil
}

// focus points the handle at the challenge now executing. every step and finding
// recorded from here belongs to it.
func (w *W) focus(cur *ChallengeRun) {
	w.cur = cur
}

// Dir returns an absolute path under the world root, creating the directory. a
// challenge naming a directory means to use one.
func (w *W) Dir(rel ...string) string {
	p := w.Path(rel...)
	w.requireContained(p)
	if err := os.MkdirAll(p, 0o755); err != nil {
		w.faultf("preparing world directory %s: %v", p, err)
	}
	return p
}

// Path returns an absolute path under the world root without creating anything.
// an already-absolute argument passes through, so a path handed out by Dir can be
// handed back.
//
// the result is required to stay inside the world. a suite path that climbs out —
// through traversal or an unrelated absolute path — is a broken harness input, and
// it faults rather than proceeding: outside the world lie the harness's own session
// state and the checkpoint images, and a write that lands there corrupts the world
// the verdict is supposed to be about. containment is lexical, so an assertion can
// still be made about a symlink that points anywhere it likes.
func (w *W) Path(rel ...string) string {
	joined := filepath.Join(rel...)
	root := filepath.Clean(w.home.world())
	p := filepath.Join(root, joined)
	if filepath.IsAbs(joined) {
		p = filepath.Clean(joined)
	}
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		w.faultf("path %q resolves to %s, outside the world at %s", joined, p, root)
	}
	return p
}

// requireContained resolves a path's deepest existing ancestor and requires it to
// stay inside the world.
//
// lexical containment is enough to *observe* a path — an assertion about a symlink
// pointing anywhere is a legitimate thing to make. a *write* needs more, because
// the world under test is full of links the product put there: a `world/media`
// pointing at somewhere else makes `media/config.yaml` lexically innocent and
// physically outside, and a write that lands there can reach the harness's own
// session state or the checkpoint images. that is invalid harness input, and it
// faults before anything is created.
func (w *W) requireContained(p string) {
	root, err := filepath.EvalSymlinks(w.home.world())
	if err != nil {
		w.faultf("resolving the world at %s: %v", w.home.world(), err)
	}
	probe := p
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
				w.faultf("path %q resolves through a link to %s, outside the world at %s", p, resolved, root)
			}
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			w.faultf("resolving %s: %v", probe, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			w.faultf("path %q has no existing ancestor inside the world at %s", p, root)
		}
		probe = parent
	}
}

// BinDir is where build artifacts live: beside the world, never inside it, so no
// checkpoint ever contains a binary.
func (w *W) BinDir() string {
	return w.home.bin()
}

// Put deposits a value into the world for a later challenge to collect.
//
// a value held in a local is challenge-local by nature — resume starts execution
// partway down the corridor, and a variable an earlier challenge assigned was
// never assigned in a resumed process. the deposit is a file under the world root,
// so it rides the checkpoint and a resumed run restores context along with state.
func (w *W) Put(key, value string) {
	w.kv[key] = value
	if err := writeHarnessMap(filepath.Join(w.home.harness(), kvName), w.kv); err != nil {
		w.faultf("depositing %q: %v", key, err)
	}
	w.step(StepFs, fmt.Sprintf("put %s", key), value)
}

// Get collects a value an earlier challenge deposited. a deposit that never
// happened is harness-owned state the harness cannot supply, so it faults rather
// than handing back an empty string that would fail somewhere confusing.
func (w *W) Get(key string) string {
	v, ok := w.kv[key]
	if !ok {
		w.faultf("no deposit named %q in the world", key)
	}
	w.step(StepFs, fmt.Sprintf("get %s", key), v)
	return v
}

// Setenv sets an environment variable applied to every invocation from here on. it
// is world state, deposited beside the values, so it rides the checkpoint and
// rolls back with a restore.
func (w *W) Setenv(key, value string) {
	w.env[key] = value
	if err := writeHarnessMap(filepath.Join(w.home.harness(), envName), w.env); err != nil {
		w.faultf("setting world environment %q: %v", key, err)
	}
	w.step(StepFs, fmt.Sprintf("setenv %s", key), value)
}

// envPairs renders the world environment as os/exec expects it, in a stable order.
func (w *W) envPairs() []string {
	keys := make([]string, 0, len(w.env))
	for k := range w.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+w.env[k])
	}
	return out
}

// FreePort allocates an unused TCP port by binding and releasing it. a fixed port
// is a collision waiting for whatever else the host happens to run.
func (w *W) FreePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		w.faultf("allocating a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	// releasing the probe is harness-owned setup. a listener that will not close
	// leaves the port occupied, and the fixture that later fails to bind it would
	// be reported as a product that never became ready.
	if err := l.Close(); err != nil {
		w.faultf("releasing the probe listener on port %d: %v", port, err)
	}
	return port
}

// WriteFile writes a file into the world. it is a harness action — setup, fault
// injection, a config the vocabulary authors — so a failure is a harness fault.
func (w *W) WriteFile(rel string, data []byte) {
	p := w.Path(rel)
	w.requireContained(p)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.faultf("preparing %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		w.faultf("writing %s: %v", p, err)
	}
	w.step(StepFs, fmt.Sprintf("write %s", rel), fmt.Sprintf("%d bytes", len(data)))
}

// ReadFile reads product-owned bytes out of the world. the value travels onward,
// so bytes that are not there are a break rather than a counted assertion: the
// flow that depended on them is severed either way, and saying so at the moment of
// the mismatch is what keeps the class honest.
func (w *W) ReadFile(rel string) []byte {
	p := w.Path(rel)
	data, err := os.ReadFile(p)
	if err != nil {
		w.breakf("reading %s: %v", rel, err)
	}
	w.step(StepFs, fmt.Sprintf("read %s", rel), fmt.Sprintf("%d bytes", len(data)))
	return data
}

// ReadYAML decodes a dd-marshaled state file from the world into a mirror struct.
//
// the mirror is the vocabulary's own struct, never the product's type. the
// duplication is deliberate: drift between mirror and product is the suite
// catching a format change on the shipped surface, and it is signal only because
// the mismatch lands in the right class at the moment it happens — an invalid
// decode request is the harness's fault, product bytes that will not fit the
// mirror are a break.
//
// the binding posture is dd's forgiving default, because a mirror is deliberately
// narrower than the state file it reads and a strict posture would reject every
// product field the mirror does not care about. what makes drift signal is the
// mirror's own declaration: a field the suite depends on carries
// `dd:"+required"`, so a rename or a removal on the shipped surface fails the bind
// rather than quietly yielding a zero value.
func (w *W) ReadYAML(rel string, into any) {
	w.requireDecodeTarget(into)
	p := w.Path(rel)
	data, err := os.ReadFile(p)
	if err != nil {
		w.breakf("reading %s: %v", rel, err)
	}
	if err := dd.BindYAML(into, data); err != nil {
		w.breakf("decoding %s into %T: %v", rel, into, err)
	}
	w.step(StepFs, fmt.Sprintf("read %s", rel), fmt.Sprintf("decoded into %T", into))
}

// ReadJSON is ReadYAML's sibling for JSON-marshaled state, under the same tiers.
func (w *W) ReadJSON(rel string, into any) {
	w.requireDecodeTarget(into)
	p := w.Path(rel)
	data, err := os.ReadFile(p)
	if err != nil {
		w.breakf("reading %s: %v", rel, err)
	}
	if err := dd.BindJSON(into, data); err != nil {
		w.breakf("decoding %s into %T: %v", rel, into, err)
	}
	w.step(StepFs, fmt.Sprintf("read %s", rel), fmt.Sprintf("decoded into %T", into))
}

// Exists asserts a path is present. a path that is simply not there is the
// product's failure to produce it; a path the harness cannot stat at all is the
// harness's problem, and it says so rather than reporting on a world it could not
// observe.
func (w *W) Exists(rel string) {
	p := w.Path(rel)
	w.step(StepFs, fmt.Sprintf("exists %s", rel), "")
	if _, err := os.Lstat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			w.failf("expected %s to exist", rel)
			return
		}
		w.faultf("checking %s: %v", rel, err)
	}
}

// Absent asserts a path is not present. absence is only ever ENOENT: any other
// error means the harness could not tell, and reading that as "not there" is how a
// suite passes on a world it never saw.
func (w *W) Absent(rel string) {
	p := w.Path(rel)
	w.step(StepFs, fmt.Sprintf("absent %s", rel), "")
	_, err := os.Lstat(p)
	switch {
	case err == nil:
		w.failf("expected %s to be absent", rel)
	case errors.Is(err, fs.ErrNotExist):
		return
	default:
		w.faultf("checking %s: %v", rel, err)
	}
}

// SameBytes asserts two paths in the world hold identical content, streaming the
// comparison so a large object costs no more memory than a small one.
func (w *W) SameBytes(a, b string) {
	w.step(StepFs, fmt.Sprintf("same-bytes %s %s", a, b), "")
	same, missing, err := sameBytes(w.Path(a), w.Path(b))
	switch {
	case err != nil:
		w.faultf("comparing %s and %s: %v", a, b, err)
	case missing != "":
		w.failf("expected %s to exist for comparison with its counterpart", missing)
	case !same:
		w.failf("expected %s and %s to hold identical bytes", a, b)
	}
}

// Note records authored prose into the narrative. the provenance behind an odd
// check is load-bearing knowledge, and it belongs in the transcript beside the
// check it explains.
func (w *W) Note(format string, args ...any) {
	w.step(StepNote, fmt.Sprintf(format, args...), "")
}

// Fail records an assertion-class finding. the corridor continues through it: a
// wording mismatch severs no dependent flow.
func (w *W) Fail(format string, args ...any) {
	w.failf(format, args...)
}

// step appends an action to the challenge now executing and hands it back so the
// caller can complete it.
func (w *W) step(kind StepKind, label, detail string) *Step {
	s := &Step{Kind: kind, Label: label, Detail: detail, At: time.Now()}
	w.cur.Steps = append(w.cur.Steps, s)
	return s
}

// stepIndex is the position of the step now in flight, or -1 outside one.
func (w *W) stepIndex() int {
	return len(w.cur.Steps) - 1
}

// record appends a finding against the step in flight and, for every class but
// assertion, unwinds the invocation.
func (w *W) record(class FindingClass, message, detail string) {
	w.recordAt(class, w.stepIndex(), message, detail)
}

// recordAt appends a finding against a named step. an implicit expectation is
// resolved after later steps have already been taken, so it says which step it is
// about rather than which one happened to be last.
func (w *W) recordAt(class FindingClass, step int, message, detail string) {
	f := &Finding{Class: class, Message: message, Detail: detail, Step: step, At: time.Now()}
	w.cur.Findings = append(w.cur.Findings, f)
	// a run already on its way out records what it finds during cleanup rather
	// than re-entering the unwind it is in the middle of.
	if class.Terminal() && !w.unwinding {
		panic(unwind{class: class})
	}
}

// interrupted reports that the harness itself is ending the run.
//
// the recorded flag and the context have to be read together. whoever cancels the
// context is the harness, and a child that dies because of it died by the
// harness's hand — reading only the flag would let the same interruption arrive as
// a product crash on the command channel and a product break on the wire.
func (w *W) interrupted() bool {
	return w.cancelled.Load() || w.ctx.Err() != nil
}

// pendingBreak is a wire failure waiting to learn what it was. a fixture that
// stopped answering may be refusing and healthy, or a moment from dying, and only
// cleanup can tell the difference.
type pendingBreak struct {
	instance string
	message  string
	step     int
}

// deferBreak leaves the invocation now and settles the finding at cleanup.
func (w *W) deferBreak(instance, message string) {
	w.pendingBreaks = append(w.pendingBreaks, pendingBreak{instance: instance, message: message, step: w.stepIndex()})
	if !w.unwinding {
		panic(unwind{class: ClassBreak})
	}
}

// resolvePendingBreaks records the wire failures cleanup did not turn out to
// explain. a fixture whose collapse has already been attributed keeps its one
// finding at the higher tier; a wire that genuinely turned a request down gets its
// break, attributed to the step that made the request.
func (w *W) resolvePendingBreaks() {
	pending := w.pendingBreaks
	w.pendingBreaks = nil
	for _, pb := range pending {
		if w.crashedFixtures[pb.instance] {
			continue
		}
		w.recordAt(ClassBreak, pb.step, pb.message, "")
	}
}

// recordQuiet appends a finding without unwinding, for the paths where a different
// finding is about to end the run and both statements have to survive.
func (w *W) recordQuiet(class FindingClass, message, detail string) {
	w.cur.Findings = append(w.cur.Findings, &Finding{
		Class: class, Message: message, Detail: detail, Step: w.stepIndex(), At: time.Now(),
	})
}

// abandon leaves the invocation without recording a new finding.
//
// harness cancellation is one interruption and earns one finding, recorded where
// the interruption was received rather than again at every call it cut short. what
// it does mark is the run itself: an interrupted run is an invalid run, and the
// model has to say so on its own rather than depending on a face remembering to.
func (w *W) abandon() {
	w.run.Interrupted = true
	if !w.unwinding {
		panic(unwind{class: ClassFault})
	}
}

// faultf records a harness fault: the harness or the suite itself is broken.
//
// it does not return — except once the run is already unwinding, when a finding is
// recorded rather than re-entering the unwind. every call site reached during
// cleanup therefore has to choose its own next step rather than assume there is
// not one.
func (w *W) faultf(format string, args ...any) {
	w.record(ClassFault, fmt.Sprintf(format, args...), "")
}

// breakf records a break: a product-surface failure severing dependent flow.
func (w *W) breakf(format string, args ...any) {
	w.record(ClassBreak, fmt.Sprintf(format, args...), "")
}

// crashf records a product crash.
func (w *W) crashf(format string, args ...any) {
	w.record(ClassCrash, fmt.Sprintf(format, args...), "")
}

// failf records an assertion failure.
func (w *W) failf(format string, args ...any) {
	w.record(ClassAssertion, fmt.Sprintf(format, args...), "")
}

// requireDecodeTarget refuses a decode the harness cannot perform.
//
// an unusable destination is a broken harness input — a suite programming error —
// and it has to be caught before the bytes are read. letting a non-pointer or a
// typed-nil pointer reach the binder turns the binder's complaint about the
// destination into a complaint about the product: exit 1 instead of 2, and an
// operator pointed at a shipped surface that never misbehaved.
func (w *W) requireDecodeTarget(into any) {
	v := reflect.ValueOf(into)
	if into == nil || v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		w.faultf("decoding into %T: a destination must be a non-nil pointer to a mirror struct", into)
	}
}

// readHarnessMap loads one of the harness-owned deposit files, treating absence as
// empty and anything else as state the harness cannot trust.
func readHarnessMap(path string) (map[string]string, error) {
	f, err := dd.NewYAMLFile[kvFile](path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("reading harness state %s: %w", path, err)
	}
	if f.Values == nil {
		return map[string]string{}, nil
	}
	return f.Values, nil
}

// writeHarnessMap persists one of the harness-owned deposit files.
func writeHarnessMap(path string, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("preparing %s: %w", filepath.Dir(path), err)
	}
	if err := dd.UnbindYAMLFile(&kvFile{Values: values}, path); err != nil {
		return fmt.Errorf("writing harness state %s: %w", path, err)
	}
	return nil
}

// sameBytes streams a comparison of two files. it reports which path was missing
// rather than folding absence into inequality, so the caller can say the true
// thing about what it found.
func sameBytes(a, b string) (same bool, missing string, err error) {
	fa, err := os.Open(a)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, a, nil
		}
		return false, "", err
	}
	defer fa.Close()

	fb, err := os.Open(b)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, b, nil
		}
		return false, "", err
	}
	defer fb.Close()

	sa, err := fa.Stat()
	if err != nil {
		return false, "", err
	}
	sb, err := fb.Stat()
	if err != nil {
		return false, "", err
	}
	if sa.Size() != sb.Size() {
		return false, "", nil
	}

	bufA := make([]byte, 64*1024)
	bufB := make([]byte, 64*1024)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, "", nil
		}
		endA := errA == io.EOF || errA == io.ErrUnexpectedEOF
		endB := errB == io.EOF || errB == io.ErrUnexpectedEOF
		if endA || endB {
			return endA == endB, "", nil
		}
		if errA != nil {
			return false, "", errA
		}
		if errB != nil {
			return false, "", errB
		}
	}
}
