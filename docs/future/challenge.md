# challenge v1

*Spec. Destined for `github.com/michaelquigley/challenge`, `docs/future/challenge-v1.md`. Born in a design session 2026-08-04; reef is the first consumer. Status: converged — mercurius `ready_to_build`, nine rounds, 2026-08-06; awaiting implementation. Work order beside this document.*

## Vision

Complex systems earn trust by surviving pressure, and the pressure that matters is longitudinal. An estate is not exercised by a thousand independent unit tests — it is exercised by a *life*: initialized, grown, damaged, corrected, recovered, and interrogated at every step along the way. The assertions that guard reef today live in exactly that shape — one estate passing through eighteen sections of mutation and repair — and those assertions are load-bearing for the project and for the real hq estate behind it. The shape is right. The mechanism is wrong: a 370-line bash script whose structure lives in comments, whose extraction is regex against formats our own libraries emit, and whose failures cost a full re-run to reproduce.

challenge is a Go library for building and maintaining these full-system pressure suites. The vocabulary is the model: a project composes named **challenges** — ordeals the system must survive — into its **gauntlet**, an ordered corridor run end to end against one living world. The system takes hits and must come through whole.

challenge is for Go-based systems that follow the house conventions — df-shaped CLIs and servers emitting dl output and dd-marshaled state. reef is the first consumer and the port of its existing suite is v1's acceptance test. flo adopts immediately after. agora, zrok, and sterling have wanted this shape for a long time and are the parties that justify challenge being born standalone rather than extracted later.

## The Problem

The reference pathology is `reef/test/e2e/exercise.sh`. What it does is exactly right and must be preserved without loss: build the real binary, stand up a scratch world of simulated media, drive the full CLI surface through a long narrative of estate life, assert on rendered output, exit codes, and filesystem truth, with a suite-wide server fixture underneath. What it costs is the problem:

- Structure lives in comments. Section boundaries, state dependencies ("re-add before the goal stages so the backup group is present for the rest of the suite"), rendering caveats — all prose, none enforced, all load-bearing.
- Extraction is reverse-engineering. `grep -oP '"msg":"\K'` against dl JSON; `awk` against dd YAML. The script fights, with regexes, formats the product's own libraries speak natively.
- A failure in section fourteen costs a full re-run to reach section fourteen again. There is no resume, no partial run, no navigation.
- The helper layer (`out`, `raw`, `check`, `ok`, `eval`) is an ad-hoc test framework in the language least suited to being one. Quoting hazards and `eval` sit inside the thing guarding the estate.
- Harness faults and assertion failures blur. A broken setup step cascades into confusing downstream failures instead of aborting loudly.
- It is very hard to read, and these suites are meant to be read for years.

## The Model

Four nouns carry the design.

**World.** The singular, living environment a gauntlet runs against: a root directory holding the workspace, the simulated media, the store, everything. The world is just a filesystem — that fact is load-bearing, because it makes checkpointing, inspection, and cleanup ordinary file operations. Its home is stable and project-local — a gitignored `.challenge/<gauntlet>/` beside the suite, *declared, not discovered*: the gauntlet carries its home as an explicit absolute path and every face resolves the same world regardless of working directory — because resume depends on the path: products record absolute paths inside their world (reef's store does, from the first `container init`), so a checkpoint restored anywhere but its original root is dead on arrival. The world is the product's *state*, never its artifacts: building the binary under test happens outside the model — in the suite's own `main()` or the consumer's Makefile — and the binary lives beside the world, not inside it, so no checkpoint ever contains one. Resume therefore presses current code against restored state, which is both the debug loop (edit the product, resume at the failing challenge) and a quiet standing pressure test of its own. The world also supervises long-lived processes (the server fixture): started with a readiness probe, logs captured, shut down or bounced under harness control. Fixture startup carries the error tiers: a spawn failure or an invalid probe is a harness fault; a process that dies while starting is a crash-class finding; a process that lives but never becomes ready is a product-surface finding. And both product-caused arms share one outcome: a startup failure that leaves the requested fixture unavailable ends the run — the finding retained at its tier, remaining challenges recorded as not-run. Where the checkpoint lands depends on where the failure struck: a failure during a challenge publishes no checkpoint for the broken challenge, while a failure during the boundary's automatic restart *keeps* the just-published checkpoint — that snapshot is a truthful closed world after a completed challenge — and the finding attributes to the challenge that never got its fixture, so resume from the retained boundary retries the restart. The product failed to become operational through its shipped surface, and one honest finding beats a corridor of cascade against a fixture that does not exist. One world per run. There is deliberately no parallelism — the world is singular by design; the narrative is the point.

**Challenge.** The unit of authorship: a named Go function receiving the world, performing actions through the shipped surface, and recording findings. Challenges are ordered; each inherits the world its predecessor left behind. The challenge is where the longitudinal narrative lives — pressure tests compose across challenges the way the estate-corrections and stale-snapshot assertions in the reef suite depend on accumulated history. Challenges are Go, full stop: capture, loops, byte-compares, file mutation as setup — all ordinary code, debuggable with ordinary tools.

**Gauntlet.** A project's ordered registry of challenges plus world configuration. Defined in code, run as a whole or navigated by name.

**Run.** One execution: a run model recording every step, every finding, every verdict — pure data, rendered by separate walkers (console reporter, transcript renderer, go-test adapter).

## The Authoring Surface

The register goal: a challenge should read like what a user does, with nothing hidden. The surfaces below are illustrative — final shapes belong to the planning phase — but the register they establish is a design decision.

**Command literals.** Commands are written as the literal a user would type, with `{}` placeholders substituting arguments as whole argv tokens — no shell, no splitting, no quoting hazards:

```go
w.Run("reef goal restore {} --archive important", w.Dir("restore")).
    ExpectMsg("restore complete")
```

**Result objects.** Every invocation returns a result: exit code, raw stdout/stderr, and the dl messages parsed as data. Assertions are fluent methods recording findings against the run model:

```go
res := w.Run("reef estate init personal")
res.ExpectExit(1)
res.ExpectMsg("already initialized")
res.ExpectMsgOnce("already initialized")   // rendered exactly once — transport discipline
```

The verdict is total: an otherwise-unasserted result carries an implicit expectation — exit 0 for commands, success status for HTTP — displaced the moment an explicit `ExpectExit` or `ExpectStatus` is recorded. A setup step that fails is a finding even when nobody asked — precisely the loud abort the bash could never give its `>/dev/null` lines.

Crash detection is built in and evidential, not merely textual: `panic:` or `goroutine ` in any stream, a command terminated by a signal, or a supervised process found dead before its requested quiesce — each flags a crash-class finding automatically, without an explicit check per invocation. Evidence coalesces: one crash is one finding, attributed to the challenge in whose window it happened, never re-discovered by a later boundary or a resumed run. And provenance is respected: terminations the harness itself initiated — cancellation on interrupt, quiesce at a boundary, the unwind — are never crash evidence; only unsolicited deaths count. The crash tier is only worth having if it cannot be triggered by the harness's own hand. And a crash is terminal: the invocation ends as findings, and the challenge in whose window it happened publishes no checkpoint — post-crash state never enters the save-point chain (the boundary-restart exception stands: a crash there keeps the already-published closed-world checkpoint and attributes forward). Assertion failures remain the one non-terminal finding class — a wording mismatch breaks no data flow, and the corridor continues through it, exactly as the bash always has.

**Capture.** Values extracted from output mid-challenge are ordinary Go values:

```go
snap := w.Run("reef goal status --archive important").Capture(`snapshot ([0-9a-f]{12})`)
```

A capture held in a local is challenge-local by nature — `--from` starts execution partway down the corridor, and a variable an earlier challenge assigned was never assigned in a resumed process. A value a *later* challenge needs is therefore deposited into the world instead: `w.Put("old-snap", snap)` on one side, `w.Get("old-snap")` on the other. The deposit is itself a file under the world root, so it rides the checkpoint with everything else and a resumed run restores context along with state. The reference suite requires this — its stale-snapshot assertions consume a digest captured a challenge earlier — and it is what keeps resume exact rather than approximate.

Observation carries the error tiers too. A broken harness input — an invalid expression, a `Get` of a deposit that never happened, an unreadable harness-owned path — is a harness fault, not a finding. A negative filesystem assertion passes only on true absence (`ENOENT`); any other error is a harness fault, never read as "not there." Typed decoding sits under the same census: an invalid decode request is a harness fault, while product-owned bytes — a state file, a response body — that are missing or will not fit their mirror are a product-surface finding, terminal like any other break in dependent data flow. Mirror drift is signal because the harness *makes* it signal: the right tier, at the moment of the mismatch. And the channels themselves complete the census: a command or request the harness could not even issue — a malformed literal, a missing harness-owned binary, an unparseable URL — is a harness fault; a well-formed request the product's wire refused to answer — connection refused, reset, timeout at the shipped surface — is a product-surface finding, terminal like every break in dependent flow, excluded when recorded harness cancellation caused it, and coalesced with supervised-death evidence into a single crash finding rather than reported twice. Coalescing is instance-scoped: every request carries the identity of the named fixture its base URL belongs to, and only evidence bearing the same instance merges — an unrelated fixture's death stays its own finding, so a corridor with several servers keeps its counts honest. And a capture demands exactly one usable match: zero or ambiguous matches are a product-surface finding — the output did not say what the suite claims it says — and the finding is terminal for the invocation: the run unwinds as findings (exit 1, never 2), remaining challenges record as not-run, and no checkpoint publishes for the broken challenge, so resume cannot sail past the break. The rule generalizes into a named finding class — the **break**: a product-surface failure that severs dependent flow. Failed captures, decode mismatches, a refused wire, a fixture that never became ready — all breaks. A break ends the invocation, is never allowed to ripen into a false harness fault downstream, and never sends a zero value onward to fail somewhere confusing. Breaks rank below crashes and above assertions, and every renderer names them.

**HTTP channel.** The same world, interrogated over the wire. Results carry status and body, with dd-decoding available for structured assertions:

```go
w.Get("/api/v1/archives/important/status").
    ExpectStatus(200).
    ExpectBody(`"satisfied":true`)
```

The channels are two views of one living world, and cross-surface coherence is a first-class assertion pattern challenge exists to make expressible: act through the CLI, verify through the API, and assert the surfaces agree — something neither handler tests nor curl smoke can say.

**Interaction and state.** Stdin arms (`.Stdin("n\n")` for the declined-prompt path), environment overrides per invocation (the foreign-workspace probes), typed reads of dd-marshaled state files, filesystem assertions (existence, absence, byte-equality including large objects), readiness polling, and free-port allocation (the harness picks an unused port for the vocabulary to write into the world's config — a fixed port is a collision waiting for whatever else the host runs) — all part of the core vocabulary, because all of them appear in or are demanded by the reference suite.

## The Rendered Surface Is the Contract

The bash suite gets one thing exactly right and the port must not lose it: assertions target the *human-rendered* surface. Substring checks against go-pretty tables, message wording, tailed job logs — these pin the contract the user actually experiences. Wording is contract. Parsed dl messages and dd-decoded bodies are conveniences layered on top of the rendered surface, never replacements for it. One precision: the surface the suite sees is the *piped* rendering — dl selects JSON transport when stdout is not a terminal, and a harness subprocess never is. That is the surface cron and CI live on, so it is shipped surface in its own right; message wording is identical across both modes, the harness relies on the non-TTY default rather than forcing an override, and the TTY-pretty renderer stays out of scope. Exit codes, single-render transport checks, no-stack-trace checks, and the real-stdin prompt arm are first-class citizens, because the things they guard — how failure *feels* at the shell — are product behavior.

The complementary boundary also stays drawn: deterministic policy edges (the mutation-during-runs rules, and their kin) belong to handler and unit tests inside the product. challenge owns the wire-level and shell-level truth of a real, aging world. It does not race tiny windows to catch mid-flight states; that way lies flake.

## Checkpoints and Resume

Because the world is a filesystem, the harness snapshots it between challenges. A failure in the fourteenth challenge resumes from the thirteenth's checkpoint — `--from` names the challenge; `--only` runs one challenge from its predecessor's snapshot. This is the single largest operational improvement over the bash, and it is structural: the full-re-run cost disappears without sacrificing the narrative.

Checkpointing is per-boundary policy, defaulted on: every boundary snapshots unless a challenge opts out, and the opt-out is visible in the code at the boundary that made the trade. Resume generalizes to the save-point model, with the coordinate system stated exactly: checkpoints are *post-challenge* boundaries, and `--from` restores the greatest checkpoint **strictly before** its target — never the target's own — replaying the intervening challenges live, then executing the target against its predecessor's world. The fresh world is itself the zeroth save point (a genesis checkpoint taken at reset, before the first challenge), so the corridor's first challenge resolves like any other rather than being a special case. And restore moves a *frontier*: a successful restore at a boundary invalidates every checkpoint beyond it before execution continues — resuming branches history, and the abandoned branch's save points must not remain selectable, or a later navigation restores a timeline that no longer exists and reports green against the wrong world.

The residue is self-describing. A fresh run mints a *session identity* — the world generation — and records the corridor's shape beside it: the ordered challenge names and their checkpoint policies. Every invocation mints a *run identity* of its own, stamped through the run model, the transcript's attempt sections, and the checkpoints it publishes. Navigation validates before it restores: the current gauntlet's prefix through the requested target must match the session's recorded prefix exactly — a challenge inserted, removed, renamed, or reordered since the residue was made is a harness fault that names the session and the divergence point, with `--clean` as the way forward. A changed *future* is different from a changed past: when the prefix through the target matches but the suffix has moved, the resume is valid and branches — and the session rebases, its recorded shape replaced with the current gauntlet's after the frontier clears the abandoned future and before replay publishes anything, so record and world move together. Suites are meant to evolve; checkpoints from a corridor that no longer exists must refuse, not resolve — and checkpoints from the corridor that now exists must resolve, not refuse. Under the default the replay span is zero; where an author has traded snapshots away for cost, resume pays in replay instead of full re-run. The bounce is not part of the trade — quiescence happens at every boundary regardless (below), snapshot or no snapshot.

Snapshots are reflink-first copies (`--reflink=auto` semantics): copy-on-write where the filesystem offers it, an honest full copy elsewhere. Expectations calibrated: none of our operating environments run a reflink-capable filesystem, so the full copy *is* the working cost — the reflink attempt is kept because it costs nothing, not because we plan to collect on it. The per-challenge opt-out and the measured cost of the real port are the operative mitigations. Hardlink cloning is available as an opt-in optimization for large-blob trees, with the hazard named: hardlinks share inodes, so a product that writes a file in place silently mutates history. The default must never be able to lie. The reef suite itself is disqualified from hardlink mode and stays that way: its narrative mutates tree files in place — truncating rewrites, permission flips — so hardlinked checkpoints would rewrite history through the shared inode. The first person to feel full-copy cost on ext4 must not reach for this opt-in on reef's gauntlet.

The world keeps one generation of history. A fresh full run resets the tree and starts clean; a failed or aborted run leaves the world, its checkpoints, and the partial transcript in place — that residue *is* the debugging session `--from` resumes — and `--clean` discards it explicitly. The stable path means a concurrent second run of the same gauntlet would collide with the first; a lock turns that collision into a loud refusal, never a second world at a mangled path. The lock is anchored *beside* the gauntlet's tree, outside everything reset, restore, and `--clean` mutate — deleting a locked file orphans the held inode and lets a second process lock a fresh one, so the guard must never live inside the thing it guards. Every lifecycle operation acquires the lock first and mutates only the tree's children.

Long-lived processes do not span checkpoints — the harness quiesces them at every challenge boundary: supervised processes stop, the snapshot is taken against a closed world, and they restart before the next challenge begins. The bounce is not a workaround — it is itself a standing pressure test (the estate must survive its operating process restarting, every boundary, every run), and it is load-bearing twice over: no snapshot is ever taken under a live writer holding open state, so the copy cannot capture a torn store; and a resumed run reaches every challenge through the same bounce a full run does, so the two can never disagree about the world a challenge starts in. Resume is not a special case — it is the ordinary boundary, entered from a restored snapshot. And the unwind is unconditional: a run that ends for *any* reason — clean, findings, harness fault, or interruption — quiesces its processes before it exits, and a cleanup failure is itself a harness fault. Interruption is a first-class ending: the standalone face catches SIGINT/SIGTERM, cancels in-flight work, completes the same unwind, and exits 2 — an interrupted run is an invalid run, and it says so instead of leaving a live writer behind a released lock. Harness-spawned processes run in their own process groups, isolated from the terminal's foreground group, so a keyboard interrupt reaches only the harness: provenance is recorded first, and children die by the harness's recorded hand — never by a stray tty signal that would masquerade as a product crash. The residue is only a debugging session if nothing is still writing to it.

## The Transcript

The run model renders to a transcript: a readable document of the run — per challenge, the literal commands, the salient output, the verdicts. The transcript is a *projection of execution*, generated from ground truth, never a second source that can drift. It is written incrementally, so a failed or aborted run leaves the partial transcript as the reproduction narrative — the document you read when the guardian suite goes red. Verdicts are per-invocation: a run — full or resumed — computes its exit status only from work it executed, and the transcript separates attempts visibly, a resumed run opening a new attempt section against its restored boundary rather than appending silently into the prior narrative. The document stays honest about what ran when. This is the same move the wider practice makes everywhere: the readable artifact is synthesized from reality, not maintained beside it.

## The Runner: One Engine, Two Faces

The engine is a plain Go API — build a gauntlet, run it, receive a run model. Two thin faces sit on top:

**Standalone (primary).** The project's suite is its own binary — `package main` calling `challenge.Main(g)` — with flags for navigation (`--from`, `--only`, `--list`), world lifecycle (`--clean`), transcript output, and verbose reporting. Exit codes carry the verdict in the same wire-status philosophy reef itself uses: `0` clean, `1` findings, `2` harness fault. System suites are consumable by `make e2e`, cron, and CI without the `go test` machinery in the way.

**go test (secondary).** A thin adapter maps the same run onto `testing.T` for IDE integration and unified CI reporting where wanted. It lives behind a build tag (`//go:build challenge`), so `go test ./...` never trips a system suite by accident — unit tests and system tests do not mix by mechanism, not by convention. This face runs the gauntlet whole: navigation belongs to the standalone face alone, and stays there. And it always runs `-count=1`: the import seam keeps the suite package independent of the product's source, so Go's test cache would otherwise return a stale green for a product it never re-ran. A cached verdict is no verdict — omitting `-count=1` is unsupported for system-verdict use.

Both faces are walkers over the run model; neither contains logic of its own.

## Core and Vocabulary

challenge stays deliberately small: world, process supervision, command and HTTP runners, result objects, challenge composition, checkpoints, the run model, and its renderers. Small enough to audit matters more than featureful — these suites guard real estates, and a clever harness that can silently pass is worse than ugly bash that visibly checks.

Domain vocabulary lives with each consumer. reef keeps a vocabulary package beside its suite — estate builders, reef nouns, domain assertions — earned per-project only where repetition genuinely drags, because every helper is a place where the shipped surface disappears behind a name. flo brings its own. A helper wanted by three consumers is a candidate for promotion into core; until then it stays local.

The seam that keeps all of this end-to-end: **challenges act only through the shipped surface** — subprocess and HTTP, real stdin, real exit codes. Shared libraries (dd, dl) are imported for *parsing* what comes back, never for reaching into the product to *do* things. The bash held this discipline by being bash; challenge holds it at the review gate: the import rule — suite packages import nothing from the product tree beyond the vocabulary package, with the vocabulary package under the same rule so it cannot launder access — is a terminus quality challenge *vends* in its own repo, ready for any consumer to adopt into their canon. A consequence worth naming: typed reads of dd-marshaled state use mirror structs the vocabulary defines itself, never product types. The duplication is deliberate — drift between mirror and product is the suite catching a format change on the shipped surface. Signal, not debt.

challenge is itself df-family: dl for its own logging, dd for its own marshaling, per the house conventions.

## Scenarios

**The red run at 2am.** The reef gauntlet fails in `estate-corrections`. The console names the challenge, the step, and the finding class. The partial transcript shows the literal commands up to the fault. `--from estate-corrections` resumes from the prior checkpoint in seconds; a debugger attaches to ordinary Go. No interpreter sits between the operator and the fault.

**Porting reef.** Every section of `exercise.sh` becomes a challenge; every assertion survives, including the transport checks, the declined prompt, the 70MB round-trip, the swapped-disk guard, the dying-drive shape. The script's comments survive too — the regression provenance ("it used to pass nil progress"), the rendering caveats, the why behind every odd check are load-bearing knowledge, and they port as prose alongside the challenges rather than dying as marginalia. The bash is deleted. This port is the acceptance test of the design — if any assertion cannot be expressed, the design is wrong, not the assertion.

**Cross-surface coherence.** A challenge creates an archive through the CLI, reads it back through the API, and asserts the two surfaces agree — then launches a durability job over the wire and verifies its terminal truth on disk. The living world makes the comparison meaningful; no other test layer can express it.

**flo adopts.** flo defines its own gauntlet against the same core — its lifecycle verbs, its own vocabulary package. What flo cannot express cleanly is the signal that shapes core's first revision; what it can is the proof the core/vocabulary split landed.

## Seam Census

- **Shipped-surface contract** — *separate, enforced.* Challenges act via subprocess and HTTP only; dd/dl imported for parsing only. Enforcement: the vended import-rule terminus quality, adopted into the consumer's canon and applied at every landing stage. Revisit: never — this is the seam that keeps the suite end-to-end.
- **Error by tier** — *separate, four classes, never blurred.* Harness fault: abort loudly, run invalid, exit 2. Product crash: auto-detected, highest-order finding, terminal — no checkpoint from its window. Break: a product-surface failure severing dependent flow — failed capture, decode mismatch, refused wire, never-ready fixture — terminal, exit 1, ranked below crash. Assertion failure: counted finding, run continues, exit 1. Broken harness inputs always fault, absence is only ever `ENOENT`, and every break wears its name in every renderer. A harness bug reading as a test failure is how guardian suites quietly lie. Revisit: none.
- **Model / render** — *separate.* The run model is pure data; console reporter, transcript renderer, and go-test adapter are walkers. Three renderers exist on day one, so the separation earns its cost immediately. Revisit: none.
- **Engine / faces** — *separate.* The engine is a plain API; standalone `main()` and the go-test adapter are thin faces holding no logic. Revisit: if a face accumulates behavior, push it into the engine.
- **Checkpoint mechanism** — *reflink-first copy; hardlink opt-in with the in-place-write hazard named.* The default snapshot must be incapable of lying. Revisit: observed corruption or prohibitive cost on a real suite.
- **Core / vocabulary** — *core generic and small; domain nouns per consumer.* Promotion to core is earned at three consumers. Revisit: at flo's adoption, and again when the third consumer arrives.

## v1 Definition of Done

`exercise.sh` ported completely — every assertion preserved, challenge for section — and deleted from the reef repo. The full reef gauntlet runs standalone and under the go-test face, renders a complete transcript, and demonstrates checkpoint resume from an arbitrary challenge. The import-rule quality is vended in the challenge repo, adopted into terminus-canon, and the ported suite reviews clean against it. flo's adoption is the shakedown that follows v1, not part of it.

## Deferred (and Why)

- **Broker integration hooks.** challenge invoking mercurius or terminus programmatically waits for demonstrated need. The import-rule *quality* is not deferred: challenge vends it, our terminus-canon adopts it as part of v1, and it is how the definition of done's lint holds.
- **Golden files and rich diffing.** Adopt only if the ported corpus asks for it; substring-against-rendered-surface is the proven register today.
- **A separate `challenge` CLI.** The per-project suite binary is the runner; a wrapper binary is scope without a consumer.
- **Watch / interactive modes.** Checkpoint navigation covers the debugging loop; anything fancier waits for demonstrated need.

## Non-Goals

- **YAML or any document front-end.** Considered and dropped in the design session: capture is interwoven with flow and cannot be pushed to the edges, so a schema becomes a versioned public contract growing toward a bad programming language. Interpolated command literals close most of the readability gap; the transcript supplies the document-shaped artifact from ground truth.
- **Parallel challenge execution.** The world is singular by design; parallelism fights the point.
- **Non-Go systems.** challenge is for the Go-based, df-shaped portfolio. The plugins have their own testing culture.
- **Browser/UI testing.** SPA coverage stays at serves-the-shell smoke; real UI testing is a different tool.
