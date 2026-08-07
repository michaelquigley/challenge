# the channels, and the processes beneath them

How a challenge acts on the world: the command channel, the wire channel, and the supervised processes both of them press against. The engine that sequences challenges, the renderers, and the two faces land in a later increment.

## the command channel

A command is written as the literal a user would type, with `{}` placeholders substituting arguments as whole argv tokens:

```go
w.Run("reef goal restore {} --archive important", w.Dir("restore")).
    ExpectMsg("restore complete")
```

There is no shell. A substituted value is never split and never needs quoting, so a token containing spaces always rides a placeholder. Non-string arguments are rendered rather than refused, because a port or a job id reads better at the call site than a conversion does.

`w.Cmd(literal, args...)` arms the cases that need more than the literal, and is the same channel with the extras named:

```go
w.Cmd("reef estate recover").Stdin("n\n").Run().ExpectExit(1).ExpectMsg("declined")
w.Cmd("reef estate show").Env("REEF_WORKSPACE", elsewhere).Run().ExpectExit(1)
```

`Stdin` is the real thing on the real descriptor, because how a prompt behaves at the shell is product behavior. `Env` overrides one variable for one invocation, leaving the world's own facts alone. `Dir` runs from somewhere other than the world root.

### resolving what to run

The first token of a literal resolves in the world-adjacent `bin/` that the bootstrap builds into. There is deliberately no PATH fallback: the products these suites guard are usually installed on the machines that test them, so a fallback would let a bootstrap that quietly failed to produce a binary be papered over by whatever version happens to be on PATH — a green verdict about a build nobody made. A name with no separator resolves in `bin/` or not at all; a vocabulary that needs a system tool names its path.

The child's environment is the harness's own, then the world's deposited facts, then the invocation's overrides. Nothing about the rendering is forced. A harness subprocess is always piped, and `dl` selects its JSON transport from that fact alone — which is the surface cron and CI live on, so it is shipped surface in its own right. Forcing an override would pass the suite while quietly retiring `dl`'s own default-selection from test.

### results

Every invocation returns the exit code, the raw streams as the shell would have shown them, and the `dl` messages parsed out as data. The streams stay separate rather than being merged before anyone looks at them.

| assertion | surface |
|---|---|
| `ExpectExit(n)` | the wire status |
| `ExpectMsg` / `ExpectNoMsg` | parsed `dl` messages |
| `ExpectMsgOnce` | occurrences in the raw streams |
| `ExpectOut` / `ExpectNoOut` | raw stdout |
| `ExpectErr` / `ExpectNoErr` | raw stderr |
| `Capture(pattern)` | a value out of the raw streams |

`ExpectMsgOnce` is the transport-discipline check. An operational failure should render once, and a message that arrives twice travelled two paths to the terminal — counting the raw streams rather than the parsed messages is what catches the duplicate that parsing would fold away.

The raw-stream assertions are their own channel because tables are not `dl` messages. They render with variable cell padding, so assertions against them target single cells and header words, never phrases spanning columns.

### the verdict is total

Every completed result carries an implicit expectation — exit 0 for a command, a success status for a request — displaced the moment an explicit `ExpectExit` or `ExpectStatus` is recorded. A command nobody asked about is still expected to have succeeded, and failing that is an ordinary assertion-class finding. This is the loud abort the bash could never give its silenced setup lines, where a failing step disappeared into `/dev/null` and surfaced later as a confusing downstream failure.

The implicit expectations resolve when the challenge ends, including on the way out of a terminal finding, and each is attributed to the step it is about rather than to whichever step happened to be last.

### capture

`Capture` reads stdout then stderr — the same surface the crash scan reads, and the one where a digest appears literally inside the `dl` line that carries it, so no parsed-message indirection is needed. It returns the first capture group when the pattern has one and the whole match otherwise.

Exactly one usable match is required. An invalid expression is a broken harness input and faults; zero or ambiguous matches are a **break** — the output did not say what the suite claims it says — and a break is terminal, so the failure surfaces where it happened. That rule is what keeps a failed capture in one challenge from ripening into a missing deposit two challenges later, wearing the wrong class.

### what a command may leave behind

A finished command must leave nothing running in its process group. A product that backgrounds a child and exits leaves a writer nothing is supervising — it is not a declared fixture, so quiescence never reaches it, and a boundary snapshot taken while it works would copy a world still being changed. Stragglers are given a moment to finish exiting, then terminated, and the run says so at the harness's tier — but only after the command's own crash evidence has been read. A command that came apart *and* left a child behind is a product crash first and a housekeeping complaint second; reporting the fault first would hide the crash behind it. A suite that wants a long-lived process declares one.

### crash detection

Detection is automatic on every invocation, and evidential as well as textual: `panic:` or `goroutine ` in either stream, or termination by a signal. Each stream is read on its own, and the combined surface joins them with a newline — a fragment ending one stream and a fragment beginning the other were never adjacent anywhere, and the highest-order finding in the census is not one to synthesize out of two halves. A process killed outright may emit no marker at all, so the manner of the exit is evidence in its own right, and marker and exit evidence for one death coalesce into one finding.

Three things settle whether the harness explains a fixture's death, in order. An exit the harness *observed* before it ever asked for one was not its doing, whatever else is true — that test is one-directional and therefore safe, since the wait returns after the process is already gone. A death by signal is the harness's only if the harness sent that signal, and provenance is claimed only for signals that actually landed. And a normal exit is explained by the request itself rather than by its status: a shutdown path is entitled to return whatever it likes, and a product that comes apart on the way down says so with a marker, which is read separately.

Watching for the exit is not enough to settle any of it, because the harness's own wait reports a process gone only once the wait returns, which can be well after the death. So the question is asked of the kernel at the moment it matters: a pidfd becomes readable the instant its process dies and keeps answering for a zombie, so polling one says whether a fixture was already gone *before* it was signalled. That is what stops a fixture that fell over just before its boundary from being signalled, looking stopped by that signal, and losing the crash it earned.

Off linux there is no pidfd to poll, and the timing and manner-of-death rules carry the classification alone. What is lost there is the narrowest window — a fixture that dies in the instant between the probe and the signal. The posture is linux-first, where that window is closed exactly.

Cancellation is read from the recorded flag *and* from the context that carries it into every child. Whoever cancels the context is the harness, and reading only the flag would let one interruption arrive as a product crash on the command channel and a product break on the wire.

Provenance applies to the death, not to everything observed around it. A termination the harness itself initiated — cancellation on interrupt, quiesce at a boundary, the unwind — is never crash evidence, because the crash tier is only worth having if it cannot be triggered by the harness's own hand. But a panic marker is the product's own account of coming apart, and it counts whoever asked the process to stop: a fixture that panics in its shutdown path would otherwise pass for a clean close.

An interruption does not silence a fixture's crash. What provenance excludes is a termination the harness itself initiated, and that exclusion is already made per instance by the signals the fixture was actually sent — a fixture that came apart of its own accord a moment before the interrupt earned its finding, and discovering it during an interrupted cleanup is not a reason to throw the evidence away.

Crashes are terminal.

## supervised processes

A fixture is declared, not spawned ad hoc:

```go
w.Start(challenge.Fixture{
    Name:     "server",
    Literal:  "reef server run",
    BaseURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
    ReadyURL: "/api/v1/config",
})
```

The readiness wait is also where an interruption is noticed. Fixtures deliberately do not die by an automatic context kill — they die by the harness's explicit quiesce, with provenance recorded first — so the wait itself has to see cancellation and leave. Waiting out the timeout would report "never became ready": a product claim for a startup the harness cut short. For the same reason a probe the harness could never issue — an unusable base URL, a negative timeout — faults at declaration rather than spending the timeout failing and then blaming the product.

Readiness is required — by URL, by a file appearing, or both. A supervised process nobody can tell is ready is a fixture nothing can depend on, and the corridor beneath it would be racing its startup. Answering means answering *healthily*: a success or a redirect. A process that replies "not yet", or that does not recognize the path the suite named, is live rather than ready.

The declaration persists in the fixture registry inside the checkpoint image, so a resumed run knows what to bring back up — and a restore rolls it back, so a resumed run never starts a fixture that a later challenge registered. The registry is validated on load rather than merely bound: an entry that would come apart at the restart is corrupted harness-owned state, and it has to leave through the harness's tier rather than as an uncontrolled panic that would read like the harness itself crashing.

Declaring a fixture whose name is already running is a harness fault. Replacing the registration would strand the running process outside both the registry and the instance table, where quiescence cannot reach it — and a live writer nobody can stop is exactly what a boundary snapshot must never be taken around.

### the three ways starting fails

They are three different statements about who is broken:

- a spawn failure or an invalid probe declaration is the **harness or the suite** — a harness fault, exit 2
- a process that dies while starting is a **product crash**
- one that lives but never answers is a **product-surface break**

Both product-caused arms end the invocation. One honest finding beats a corridor of cascade run against a fixture that does not exist.

### the bounce

Supervised processes stop at every challenge boundary and start again before the next challenge begins. The bounce is not a workaround. It is load-bearing twice over: no snapshot is ever taken under a live writer holding open state, so a copy cannot capture a torn store; and a resumed run reaches every challenge through the same bounce a full run does, so the two can never disagree about the world a challenge starts in. It is a standing pressure test in its own right — the system under test must survive its operating process restarting, every boundary, every run.

Stopping is SIGTERM to the process group, a bounded wait, then SIGKILL **and a harness fault**. Quiescence assumes clean closes: a process holding a lock and a write-ahead log has to release them, and a world snapshotted around a process that had to be killed is not a closed one. The run says so loudly rather than snapshotting it anyway.

The leader exiting is not the same as the fixture being gone. A process it started can outlive it and keep writing into the world, so quiescence waits for the whole group to empty — a snapshot taken around a surviving descendant is exactly the torn image the bounce exists to prevent. The output window is read before the fault is raised, so a product that panicked on its way down is still on the record even when the harness has to give up on it — and reading consumes the window, so the evidence is spent there or lost. When both are true, both land: the crash is recorded without unwinding so the fault that ends the run can follow it, and the run exits 2 because the harness could not close the world.

Declaring a fixture whose name died on its own reports that death first. Replacing it silently would bury both the death and the window that explains it.

Publishing a boundary refuses when the challenge that just ran recorded a crash or a break: its window is checkpoint-ineligible, and that has to be a property of the operation rather than something control flow happens to prevent. It also refuses when the harness's own state on disk no longer matches the state the run believes: the deposits, the world environment, and the fixture registry all live inside the world, which means a misbehaving product can reach them, and faithful bytes are not enough if those bytes disagree with the run that produced them. It refuses while any fixture remains un-quiesced — the test is presence in the instance table, not liveness. Quiescence is what removes an instance, and it is also where an unsolicited death is observed and classified, so a fixture that died quietly and is merely *gone* has not been accounted for yet; publishing around it would make a checkpoint selectable from a crash window nobody has looked at.

The same rule covers the live case. That has to be a property of the operation rather than a rule a caller remembers: a process still holding open state can be mid-write while the copy walks the tree, and the resulting image would be a torn store nothing downstream could tell from a real one.

The unwind is unconditional. A run that ends for any reason stops its processes before it exits, because the residue a failed run leaves is only a debugging session if nothing is still writing to it. Findings raised during that cleanup are recorded rather than re-entering the unwind they are in the middle of.

### process groups

Every harness-spawned process — one-shot commands and supervised fixtures alike — runs in its own process group, isolated from the runner's foreground group. A keyboard interrupt therefore reaches only the harness: provenance is recorded first, and children die by the harness's recorded hand rather than by a stray tty signal that would masquerade as a product crash.

### output windows

The log is harness-owned state, created when the fixture launched. Finding it gone or unreadable is a harness fault rather than an absence of evidence — reporting "nothing to see" would let a shutdown panic disappear into a clean-looking boundary.

A fixture's output appends to one log file across every instance of it. Each start opens its window at the file's current size, so a boundary scan reads only what *this* instance wrote and the offset advances as it goes. A panic from an earlier life is never re-discovered by a later boundary or a resumed run — exactly-once evidence, which is what makes "one crash is one finding" true across a corridor rather than only within a challenge.

## the wire channel

The same living world, interrogated over HTTP:

```go
w.On("server").Get("/api/v1/archives/important/status").
    ExpectStatus(200).
    ExpectBody(`"satisfied":true`)
```

A request is reached through the fixture it belongs to rather than through a nameless default, and that fixture has to be alive. Registered but not running means supervision did not provide the process the suite declared, which is a lifecycle failure rather than a statement about the product's wire; registered and already dead is crash evidence, recorded before any request goes out. Either way, issuing anyway could reach whatever unrelated listener has since taken the address, and a green assertion against a stranger is the worst answer available. The instance identity is what keeps crash evidence honest: a corridor with several servers has to be able to say which one died, and naming it at the call site makes that identity visible rather than inferred.

`ExpectStatus`, `ExpectBody`, and `ExpectNoBody` assert the rendered surface; `Decode` binds the body into a mirror struct for structured assertions, under the same tiers a typed read of a state file carries — an invalid decode request is a harness fault, a body that will not fit the mirror is a break.

Cross-surface coherence is the pattern this channel exists to make expressible: act through the CLI, verify through the API, and assert the surfaces agree.

### transport failures

A request the harness could not even issue — an unparseable destination, a header the client refuses to send — is a harness fault. So is a readiness probe naming a scheme the client cannot speak: it parses, it has a host, and it would still spend the whole timeout failing before blaming the product for never becoming ready. A well-formed request the product's wire refused to answer is a break: the product failed at its shipped surface, and the flow that depended on the answer is severed.

When the fixture behind that wire is dead, the refusal and the death are one event, and they coalesce into one crash finding attributed to that instance rather than two reports of one thing. Coalescing is instance-scoped: only evidence bearing the same fixture's identity merges, so an unrelated fixture's death stays its own finding and a corridor with several servers keeps its counts honest.

What a refusal *means* is often not knowable at the moment it happens. A fixture that stopped answering may be refusing and healthy, or may be a moment from dying, and a grace period only narrows that — it cannot close it. So the break is deferred: the invocation ends immediately, and cleanup, which is where the fixture's fate becomes settled, decides whether this was one collapse (a crash) or a wire that genuinely turned a request down (a break). One event earns one finding either way.

The wire keeps no connections between requests. A suite bounces its servers at every boundary, and a pooled connection outlives the process on the other end of it — reusing one would mean a request landing on a socket to a server that no longer exists, a phantom failure that says nothing about the product and would arrive wearing a product class.

A failure caused by recorded harness cancellation is excluded entirely. One interruption earns one finding, recorded where the interruption was received rather than again at every call it cut short.

## the toy

`internal/toy` is a small df-shaped product the library presses against: it emits `dl` messages, writes `dd`-marshaled state, serves a trivial wire surface, and can be told to misbehave in each of the specific ways the harness claims to detect — panic, die by signal, die after a delay, panic after a delay, answer but never become ready, refuse to stop, prompt on real stdin, and drift its state format.

It exists so the engine is proven against real subprocesses — real exit codes, real stdin, a real socket, real signals — rather than against a mock of them. The test suite builds it once and installs it into each world's `bin/`, which is the same shape a consumer's bootstrap hook takes.

## known limits

Two provenance guarantees are bounded, deliberately, and both are recorded here rather than papered over.

**A descendant that leaves its process group.** Quiescence proves the fixture's process group is empty. Group membership is inherited but not permanent: a descendant that calls `setsid` escapes it and could keep writing while a boundary is copied. Closing that would require an escape-proof container — an invocation-scoped cgroup — which needs a delegated cgroup subtree the ordinary development and CI machines these suites run on do not have, and refusing to run without one would cost far more than the risk. The products these suites guard are df-shaped Go CLIs and servers that do not detach; a product that deliberately escapes supervision to keep writing is adversarial rather than merely broken. A suite that wants a long-lived process declares one.

**Exit provenance off linux.** The pidfd probe that settles whether a fixture died before it was signalled has no equivalent on other unix hosts, where the timing and manner-of-death rules carry the classification alone. The narrowest window — a fixture dying in the instant between the probe and the signal — is open there. The posture is linux-first and this is one of the places it shows; the alternative, faulting every boundary on an unsupported platform, would trade an unproven host for an unusable one.
