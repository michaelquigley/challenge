package challenge

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/michaelquigley/df/dd"
)

// requestTimeout bounds a single request against the shipped wire surface.
const requestTimeout = 30 * time.Second

// deathGrace is how long a refused request waits for a supervised fixture to be
// confirmed dead before deciding what the refusal means.
const deathGrace = 2 * time.Second

// Wire is one fixture's shipped wire surface — the same living world the command
// channel acts on, interrogated over HTTP.
//
// a request is reached through the fixture it belongs to rather than through a
// nameless default, because the instance identity is what keeps crash evidence
// honest: a corridor with several servers has to be able to say which one died.
type Wire struct {
	w        *W
	instance string
	base     string
}

// On selects a registered fixture's wire.
func (w *W) On(name string) *Wire {
	for _, spec := range w.specs {
		if spec.Name == name {
			if spec.BaseURL == "" {
				w.faultf("fixture %q declares no base URL", name)
			}
			// registered but not running means supervision did not provide the
			// process the suite declared — a lifecycle failure, not a statement
			// about the product's wire. issuing anyway could also reach whatever
			// unrelated listener has since taken the address.
			inst, live := w.instances[name]
			if !live {
				w.faultf("fixture %q is registered but not running; nothing answers for it", name)
			}
			// already dead is crash evidence, and it is worth more than whatever a
			// request would find: the address may since have been taken by something
			// else entirely, and a green assertion against a stranger is the worst
			// answer available.
			if inst.exited() && w.requireCleanWait(inst) {
				evidence := w.scanWindow(inst)
				w.crashFromDeath(inst,
					fmt.Sprintf("fixture %q is dead and cannot be interrogated", name),
					joinEvidence(w.exitEvidence(inst), evidence))
			}
			return &Wire{w: w, instance: name, base: spec.BaseURL}
		}
	}
	w.faultf("no fixture named %q is registered; the world has %s", name, w.fixtureNames())
	return nil
}

// fixtureNames renders the registered fixtures for a refusal to name.
func (w *W) fixtureNames() string {
	if len(w.specs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(w.specs))
	for _, spec := range w.specs {
		names = append(names, spec.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// HTTPResult is one request's outcome: the status and the body as the wire
// delivered them.
type HTTPResult struct {
	Method string
	Path   string
	Status int
	Body   string

	w              *W
	instance       string
	step           int
	statusAsserted bool
}

// Get requests a path.
func (wr *Wire) Get(path string) *HTTPResult {
	return wr.do(http.MethodGet, path, "", nil)
}

// Post sends a body to a path.
func (wr *Wire) Post(path, contentType string, body []byte) *HTTPResult {
	return wr.do(http.MethodPost, path, contentType, body)
}

// Put sends a body to a path.
func (wr *Wire) Put(path, contentType string, body []byte) *HTTPResult {
	return wr.do(http.MethodPut, path, contentType, body)
}

// Delete removes a path.
func (wr *Wire) Delete(path string) *HTTPResult {
	return wr.do(http.MethodDelete, path, "", nil)
}

// do issues a request against the fixture's wire.
//
// the channel carries the error census the same way the command channel does. a
// request the harness could not even issue — an unparseable destination, a
// malformed request — is a harness fault. a well-formed request the product's wire
// refused to answer is a break: the product failed at its shipped surface, and the
// flow that depended on the answer is severed. and when the fixture behind that
// wire is dead, the refusal and the death are one event, so they coalesce into one
// crash finding attributed to that instance rather than two reports of one thing.
func (wr *Wire) do(method, path, contentType string, body []byte) *HTTPResult {
	w := wr.w
	// a path that is not a path silently makes a different URL: a base of
	// http://host with "api" targets http://hostapi, which parses, resolves, and
	// never reaches the endpoint the suite meant. that is broken harness input, not
	// a product that refused a request.
	if !strings.HasPrefix(path, "/") {
		w.faultf("%s %q: a request path is absolute, beginning with /", method, path)
	}
	target := wr.base + path
	// a destination the client cannot speak to means the harness could not issue
	// the request at all, which is a different statement from a well-formed request
	// the product's wire turned down.
	if err := validateWireURL(target); err != nil {
		w.faultf("%s %s: %v", method, target, err)
	}

	step := w.step(StepHttp, fmt.Sprintf("%s %s", method, target), "")
	index := w.stepIndex()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(w.ctx, method, target, reader)
	if err != nil {
		w.faultf("%s %s: %v", method, target, err)
	}
	if contentType != "" {
		// a header the client will refuse to send means the harness could not issue
		// the request at all — which is a different statement from a well-formed
		// request the product's wire turned down.
		if !validHeaderValue(contentType) {
			w.faultf("%s %s: %q is not a header value the harness can send", method, target, contentType)
		}
		req.Header.Set("Content-Type", contentType)
	}

	client := newHTTPClient(requestTimeout)
	started := time.Now()
	resp, err := client.Do(req)
	step.Elapsed = time.Since(started)
	if err != nil {
		w.reportWireFailure(wr.instance, method, target, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		w.reportWireFailure(wr.instance, method, target, err)
	}

	res := &HTTPResult{
		Method:   method,
		Path:     path,
		Status:   resp.StatusCode,
		Body:     string(payload),
		w:        w,
		instance: wr.instance,
		step:     index,
	}
	step.Status = res.Status
	w.pending = append(w.pending, res)
	return res
}

// reportWireFailure classifies a transport failure and never returns.
//
// coalescing is instance-scoped: only evidence bearing this fixture's identity
// merges, so an unrelated fixture's death stays its own finding and a corridor
// with several servers keeps its counts honest.
//
// what a refusal *means* is often not knowable yet. a fixture that stopped
// answering may be refusing and healthy, or may be a moment from dying — and a
// grace period only narrows that, it cannot close it. so the break is deferred:
// the invocation ends now, and cleanup, which is where the fixture's fate becomes
// settled, decides whether this was one collapse (a crash) or a wire that genuinely
// turned a request down (a break). one event earns one finding either way.
func (w *W) reportWireFailure(name, method, target string, err error) {
	if w.interrupted() {
		// the harness itself cut this request short; the interruption is already
		// recorded where it was received.
		w.abandon()
	}
	if inst, ok := w.instances[name]; ok {
		select {
		case <-inst.done:
		case <-time.After(deathGrace):
		}
		if inst.exited() && w.requireCleanWait(inst) {
			evidence := w.scanWindow(inst)
			w.crashFromDeath(inst,
				fmt.Sprintf("fixture %q died and its wire stopped answering", name),
				joinEvidence(w.exitEvidence(inst), evidence, fmt.Sprintf("%s %s was refused", method, target)))
		}
		w.deferBreak(name, fmt.Sprintf("%s %s: %v", method, target, err))
	}
	w.faultf("%s %s failed and fixture %q is no longer supervised: %v", method, target, name, err)
}

// newHTTPClient builds a client that keeps no connections.
//
// a suite bounces its servers at every boundary, and a pooled connection outlives
// the process on the other end of it. reusing one would mean a request landing on
// a socket to a server that no longer exists — a phantom failure that says nothing
// about the product, and one that would arrive on the wire channel wearing a
// product class. one connection per request costs nothing here and removes the
// whole category.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

// validHeaderValue reports whether a string can travel as an HTTP header value.
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// resolveVerdict applies the implicit expectation an unasserted response carries.
// a request nobody asked about is still expected to have succeeded.
func (r *HTTPResult) resolveVerdict() {
	if r.statusAsserted || (r.Status >= 200 && r.Status < 300) {
		return
	}
	r.w.recordAt(ClassAssertion, r.step,
		fmt.Sprintf("%s %s answered %d with nothing expecting it to", r.Method, r.Path, r.Status), r.Body)
}

// ExpectStatus asserts the wire status. naming one displaces the implicit
// expectation that the request succeeded.
func (r *HTTPResult) ExpectStatus(status int) *HTTPResult {
	r.statusAsserted = true
	if r.Status != status {
		r.w.recordAt(ClassAssertion, r.step,
			fmt.Sprintf("%s %s answered %d, expected %d", r.Method, r.Path, r.Status, status), r.Body)
	}
	return r
}

// ExpectBody asserts a substring of the response body.
func (r *HTTPResult) ExpectBody(substr string) *HTTPResult {
	if !strings.Contains(r.Body, substr) {
		r.w.recordAt(ClassAssertion, r.step,
			fmt.Sprintf("%s %s body did not contain %q", r.Method, r.Path, substr), r.Body)
	}
	return r
}

// ExpectNoBody asserts a substring is absent from the response body.
func (r *HTTPResult) ExpectNoBody(substr string) *HTTPResult {
	if strings.Contains(r.Body, substr) {
		r.w.recordAt(ClassAssertion, r.step,
			fmt.Sprintf("%s %s body contained %q", r.Method, r.Path, substr), r.Body)
	}
	return r
}

// Decode binds the response into a mirror struct for structured assertions.
//
// the tiers are the same ones typed reads of state files carry: an invalid decode
// request is a harness fault, while a body that will not fit the mirror is a break.
// drift between mirror and product is the suite catching a format change on the
// shipped surface, and it is signal because the mismatch lands in the right class
// at the moment it happens.
func (r *HTTPResult) Decode(into any) *HTTPResult {
	r.w.requireDecodeTarget(into)
	if err := dd.BindJSON(into, []byte(r.Body)); err != nil {
		r.w.recordAt(ClassBreak, r.step,
			fmt.Sprintf("%s %s body will not fit %T: %v", r.Method, r.Path, into, err), r.Body)
	}
	return r
}
