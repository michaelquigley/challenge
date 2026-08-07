package challenge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toyState is a mirror of the toy's shipped state shape, declaring the field the
// suite depends on so a rename on that surface fails the bind rather than quietly
// yielding a zero value.
type toyState struct {
	Id    string `dd:"+required"`
	Label string
}

func TestTheWireIsAnotherViewOfOneWorld(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// act through the command channel, verify through the wire, and assert the two
	// surfaces agree — the comparison neither handler tests nor curl smoke can make.
	w.Run("toy state estate.yaml toy-1 personal").ExpectMsg("wrote state")
	var onDisk toyState
	w.ReadYAML("estate.yaml", &onDisk)

	var onWire toyState
	w.On("server").Get("/api/v1/state").ExpectStatus(200).ExpectBody(`"label"`).Decode(&onWire)
	assert.Equal(t, onDisk, onWire)
	assert.Empty(t, cur.Findings)

	w.On("server").Post("/api/v1/jobs", "application/json", []byte(`{"kind":"durability"}`)).
		ExpectStatus(201).
		ExpectBody(`"state":"running"`)
	assert.Empty(t, cur.Findings)
}

func TestTheWireVerdictIsTotal(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// a request nobody asked about is still expected to have succeeded.
	w.On("server").Get("/api/v1/missing")
	assert.Empty(t, cur.Findings)
	w.resolvePending()
	require.Len(t, cur.Findings, 1)
	assert.Equal(t, ClassAssertion, cur.Findings[0].Class)
	assert.Contains(t, cur.Findings[0].Message, "answered 404 with nothing expecting it to")

	// naming an expectation displaces the implicit one, which is how the suite
	// says it means to press on a failure.
	cur.Findings = nil
	w.On("server").Get("/api/v1/missing").ExpectStatus(404).ExpectBody(`"no such thing"`)
	w.resolvePending()
	assert.Empty(t, cur.Findings)
}

func TestDecodingCarriesTheSameTiersAsStateFiles(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--drift")
	w.Start(f)

	// a rename on the shipped surface is the suite catching a format change, and it
	// lands as a break rather than a zero value travelling onward.
	var mirror toyState
	unwound, class := capture(func() { w.On("server").Get("/api/v1/state").Decode(&mirror) })
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)
	require.Len(t, cur.Findings, 1)
	assert.Equal(t, "break", cur.Findings[0].Class.String(), "every renderer names the class")
}

func TestABodyThatIsNotADocumentIsABreak(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	var mirror toyState
	unwound, class := capture(func() { w.On("server").Get("/api/v1/broken").Decode(&mirror) })
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)

	// an unusable destination is still the harness's own fault, before any bytes
	// are read.
	unwound, class = capture(func() { w.On("server").Get("/api/v1/state").Decode(nil) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
}

func TestARequestAgainstADeadFixtureIsOneCrash(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--die-after 300ms")
	w.Start(f)
	time.Sleep(700 * time.Millisecond)

	// a dead fixture is crash evidence worth more than whatever a request would
	// find: the address may since have been taken by something else, and a green
	// assertion against a stranger is the worst answer available. one event, one
	// finding, at the higher tier.
	unwound, class := capture(func() { w.On("server").Get("/api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, "cannot be interrogated")
	assert.Contains(t, cur.Findings[0].Detail, "exited 7")
	assert.Equal(t, 0, w.run.Count(ClassBreak))
}

func TestCoalescingIsScopedToOneFixture(t *testing.T) {
	w, cur, _ := testW(t)
	dying, _ := serveFixture(t, "dying", "--die-after 300ms")
	survivor, _ := serveFixture(t, "survivor")
	w.Start(survivor)
	w.Start(dying)
	time.Sleep(700 * time.Millisecond)

	// a corridor with several servers has to keep its counts honest: only evidence
	// bearing the same instance identity merges, so an unrelated fixture's death
	// stays its own finding and a healthy one is never implicated.
	unwound, class := capture(func() { w.On("dying").Get("/api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassCrash, class)
	require.Len(t, cur.Findings, 1)
	assert.Contains(t, cur.Findings[0].Message, `"dying"`)
	assert.NotContains(t, cur.Findings[0].Message, "survivor")

	// the survivor is still up and still uninvolved.
	assert.False(t, w.instances["survivor"].exited())
	assert.False(t, w.instances["survivor"].crashReported)
}

func TestAWireTheHarnessCannotReachIsAFault(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// naming a fixture that was never registered is the suite being broken, not
	// the product misbehaving.
	unwound, class := capture(func() { w.On("nonesuch").Get("/api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
}

func TestAHeaderTheHarnessCannotSendIsAFault(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// a header the client refuses to send means the request was never issued, which
	// is a different statement from a well-formed request the product turned down.
	unwound, class := capture(func() {
		w.On("server").Post("/api/v1/jobs", "application/json\r\nX-Smuggled: 1", []byte(`{}`))
	})
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Equal(t, 0, w.run.Count(ClassBreak))
}

func TestADestinationTheClientCannotSpeakToIsAFault(t *testing.T) {
	w, _, _ := testW(t)

	// a fixture that proves itself by a file rather than a URL still declares a base
	// the wire will be built on, and a base the client cannot speak to means the
	// request was never issued.
	f := Fixture{
		Name:      "server",
		Literal:   "toy sleep 30s",
		BaseURL:   "ftp://127.0.0.1:9000",
		ReadyFile: "ready",
	}
	unwound, class := capture(func() { w.Start(f) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Equal(t, 0, w.run.Count(ClassBreak))
}

func TestAWireThatRefusesWhileItsFixtureLivesIsABreak(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--close-after 300ms")
	w.Start(f)
	time.Sleep(700 * time.Millisecond)

	// the listener is gone and the process is not. what the refusal means is not
	// knowable yet, so the invocation ends now and the finding waits for cleanup to
	// settle it.
	unwound, class := capture(func() { w.On("server").Get("/api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)
	assert.Empty(t, cur.Findings, "nothing is claimed until the fixture's fate is settled")

	w.shutdown()
	require.Len(t, cur.Findings, 1)
	assert.Equal(t, ClassBreak, cur.Findings[0].Class)
	assert.Equal(t, 0, w.run.Count(ClassCrash), "a wire that turned a request down is not a crash")
}

func TestARefusalThatTurnsOutToBeADeathIsOnlyACrash(t *testing.T) {
	w, cur, _ := testW(t)
	f, _ := serveFixture(t, "server", "--close-after 300ms", "--die-after 3s")
	w.Start(f)
	time.Sleep(700 * time.Millisecond)

	// the same refusal, and a fixture that turns out to have been a moment from
	// dying. a grace period only narrows that window; cleanup is what closes it.
	unwound, class := capture(func() { w.On("server").Get("/api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassBreak, class)

	time.Sleep(3 * time.Second)
	w.shutdown()

	assert.Equal(t, 1, w.run.Count(ClassCrash))
	assert.Equal(t, 0, w.run.Count(ClassBreak), "one collapse, one finding, at the higher tier")
	require.Len(t, cur.Findings, 1)
}

func TestARegisteredFixtureThatIsNotRunningIsAFault(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)
	w.Quiesce()

	// registered but not running means supervision did not provide the process the
	// suite declared — a lifecycle failure, not a statement about the product's
	// wire. issuing anyway could also reach whatever has since taken the address.
	unwound, class := capture(func() { w.On("server").Get("/api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Equal(t, 0, w.run.Count(ClassBreak))
}

func TestARequestPathIsAbsolute(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	w.Start(f)

	// a path that is not a path silently makes a different URL: a base of
	// http://host with "api" targets http://hostapi, which parses, resolves, and
	// never reaches the endpoint the suite meant.
	unwound, class := capture(func() { w.On("server").Get("api/v1/config") })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
	assert.Equal(t, 0, w.run.Count(ClassBreak))
}

func TestAReadinessPathIsAbsolute(t *testing.T) {
	w, _, _ := testW(t)
	f, _ := serveFixture(t, "server")
	f.ReadyURL = "api/v1/config"

	unwound, class := capture(func() { w.Start(f) })
	assert.True(t, unwound)
	assert.Equal(t, ClassFault, class)
}
