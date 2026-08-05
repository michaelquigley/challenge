# challenge v1

*Spec. Destined for `github.com/michaelquigley/challenge`, `docs/future/challenge-v1.md`. Born in a design session 2026-08-04; reef is the first consumer. Status: awaiting planning agent.*

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

**World.** The singular, living environment a gauntlet runs against: a temp root holding the workspace, the simulated media, the store, everything. The world is just a filesystem — that fact is load-bearing, because it makes checkpointing, inspection, and cleanup ordinary file operations. The world also supervises long-lived processes (the server fixture): started with a readiness probe, logs captured, shut down or bounced under harness control. One world per run. There is deliberately no parallelism — the world is singular by design; the narrative is the point.

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

Crash detection is built in: `panic:` or `goroutine ` in any stream flags a crash-class finding automatically, without an explicit check per invocation.

**Capture.** Values extracted from output mid-challenge are ordinary Go values:

```go
snap := w.Run("reef goal status --archive important").Capture(`snapshot ([0-9a-f]{12})`)
```

**HTTP channel.** The same world, interrogated over the wire. Results carry status and body, with dd-decoding available for structured assertions:

```go
w.Get("/api/v1/archives/important/status").
    ExpectStatus(200).
    ExpectBody(`"satisfied":true`)
```

The channels are two views of one living world, and cross-surface coherence is a first-class assertion pattern challenge exists to make expressible: act through the CLI, verify through the API, and assert the surfaces agree — something neither handler tests nor curl smoke can say.

**Interaction and state.** Stdin arms (`.Stdin("n\n")` for the declined-prompt path), environment overrides per invocation (the foreign-workspace probes), typed reads of dd-marshaled state files, filesystem assertions (existence, absence, byte-equality including large objects), and readiness polling — all part of the core vocabulary, because all of them appear in the reference suite.

## The Rendered Surface Is the Contract

The bash suite gets one thing exactly right and the port must not lose it: assertions target the *human-rendered* surface. Substring checks against go-pretty tables, message wording, tailed job logs — these pin the contract the user actually experiences. Wording is contract. Parsed dl messages and dd-decoded bodies are conveniences layered on top of the rendered surface, never replacements for it. Exit codes, single-render transport checks, no-stack-trace checks, and the real-stdin prompt arm are first-class citizens, because the things they guard — how failure *feels* at the shell — are product behavior.

The complementary boundary also stays drawn: deterministic policy edges (the mutation-during-runs rules, and their kin) belong to handler and unit tests inside the product. challenge owns the wire-level and shell-level truth of a real, aging world. It does not race tiny windows to catch mid-flight states; that way lies flake.

## Checkpoints and Resume

Because the world is a filesystem, the harness snapshots it between challenges. A failure in the fourteenth challenge resumes from the thirteenth's checkpoint — `--from` names the challenge; `--only` runs one challenge from its predecessor's snapshot. This is the single largest operational improvement over the bash, and it is structural: the full-re-run cost disappears without sacrificing the narrative.

Snapshots are reflink-first copies (`--reflink=auto` semantics): copy-on-write and effectively free on btrfs/xfs, an honest full copy elsewhere. Hardlink cloning is available as an opt-in optimization for large-blob trees, with the hazard named: hardlinks share inodes, so a product that writes a file in place silently mutates history. The default must never be able to lie.

Long-lived processes span checkpoints, so resuming bounces them against the restored world. That bounce is not a workaround — it is itself a standing pressure test: the estate must survive its operating process restarting.

## The Transcript

The run model renders to a transcript: a readable document of the run — per challenge, the literal commands, the salient output, the verdicts. The transcript is a *projection of execution*, generated from ground truth, never a second source that can drift. It is written incrementally, so a failed or aborted run leaves the partial transcript as the reproduction narrative — the document you read when the guardian suite goes red. This is the same move the wider practice makes everywhere: the readable artifact is synthesized from reality, not maintained beside it.

## The Runner: One Engine, Two Faces

The engine is a plain Go API — build a gauntlet, run it, receive a run model. Two thin faces sit on top:

**Standalone (primary).** The project's suite is its own binary — `package main` calling `challenge.Main(g)` — with flags for navigation (`--from`, `--only`, `--list`), transcript output, and verbose reporting. Exit codes carry the verdict in the same wire-status philosophy reef itself uses: `0` clean, `1` findings, `2` harness fault. System suites are consumable by `make e2e`, cron, and CI without the `go test` machinery in the way.

**go test (secondary).** A thin adapter maps the same run onto `testing.T` for IDE integration and unified CI reporting where wanted. It lives behind a build tag (`//go:build challenge`), so `go test ./...` never trips a system suite by accident — unit tests and system tests do not mix by mechanism, not by convention.

Both faces are walkers over the run model; neither contains logic of its own.

## Core and Vocabulary

challenge stays deliberately small: world, process supervision, command and HTTP runners, result objects, challenge composition, checkpoints, the run model, and its renderers. Small enough to audit matters more than featureful — these suites guard real estates, and a clever harness that can silently pass is worse than ugly bash that visibly checks.

Domain vocabulary lives with each consumer. reef keeps a vocabulary package beside its suite — estate builders, reef nouns, domain assertions — earned per-project only where repetition genuinely drags, because every helper is a place where the shipped surface disappears behind a name. flo brings its own. A helper wanted by three consumers is a candidate for promotion into core; until then it stays local.

The seam that keeps all of this end-to-end: **challenges act only through the shipped surface** — subprocess and HTTP, real stdin, real exit codes. Shared libraries (dd, dl) are imported for *parsing* what comes back, never for reaching into the product to *do* things. The bash held this discipline by being bash; challenge holds it mechanically — an import rule (suite packages import nothing from the product tree beyond the vocabulary package) that is lintable and a natural quality for terminus to carry.

challenge is itself df-family: dl for its own logging, dd for its own marshaling, per the house conventions.

## Scenarios

**The red run at 2am.** The reef gauntlet fails in `estate-corrections`. The console names the challenge, the step, and the finding class. The partial transcript shows the literal commands up to the fault. `--from estate-corrections` resumes from the prior checkpoint in seconds; a debugger attaches to ordinary Go. No interpreter sits between the operator and the fault.

**Porting reef.** Every section of `exercise.sh` becomes a challenge; every assertion survives, including the transport checks, the declined prompt, the 70MB round-trip, the swapped-disk guard, the dying-drive shape. The script's comments survive too — the regression provenance ("it used to pass nil progress"), the rendering caveats, the why behind every odd check are load-bearing knowledge, and they port as prose alongside the challenges rather than dying as marginalia. The bash is deleted. This port is the acceptance test of the design — if any assertion cannot be expressed, the design is wrong, not the assertion.

**Cross-surface coherence.** A challenge creates an archive through the CLI, reads it back through the API, and asserts the two surfaces agree — then launches a durability job over the wire and verifies its terminal truth on disk. The living world makes the comparison meaningful; no other test layer can express it.

**flo adopts.** flo defines its own gauntlet against the same core — its lifecycle verbs, its own vocabulary package. What flo cannot express cleanly is the signal that shapes core's first revision; what it can is the proof the core/vocabulary split landed.

## Seam Census

- **Shipped-surface contract** — *separate, enforced.* Challenges act via subprocess and HTTP only; dd/dl imported for parsing only. Enforcement: import lint on suite packages; terminus quality. Revisit: never — this is the seam that keeps the suite end-to-end.
- **Error by tier** — *separate, three classes, never blurred.* Harness fault: abort loudly, run invalid, exit 2. Assertion failure: counted finding, run continues, exit 1. Product crash: auto-detected, highest-order finding. A harness bug reading as a test failure is how guardian suites quietly lie. Revisit: none.
- **Model / render** — *separate.* The run model is pure data; console reporter, transcript renderer, and go-test adapter are walkers. Three renderers exist on day one, so the separation earns its cost immediately. Revisit: none.
- **Engine / faces** — *separate.* The engine is a plain API; standalone `main()` and the go-test adapter are thin faces holding no logic. Revisit: if a face accumulates behavior, push it into the engine.
- **Checkpoint mechanism** — *reflink-first copy; hardlink opt-in with the in-place-write hazard named.* The default snapshot must be incapable of lying. Revisit: observed corruption or prohibitive cost on a real suite.
- **Core / vocabulary** — *core generic and small; domain nouns per consumer.* Promotion to core is earned at three consumers. Revisit: at flo's adoption, and again when the third consumer arrives.

## v1 Definition of Done

`exercise.sh` ported completely — every assertion preserved, challenge for section — and deleted from the reef repo. The full reef gauntlet runs standalone and under the go-test face, renders a complete transcript, and demonstrates checkpoint resume from an arbitrary challenge. The import lint holds. flo's adoption is the shakedown that follows v1, not part of it.

## Deferred (and Why)

- **mercurius / terminus integration hooks.** Let the tool exist before the brokers learn about it. The import-rule quality can enter terminus canon once the pattern has run on real reviews.
- **Golden files and rich diffing.** Adopt only if the ported corpus asks for it; substring-against-rendered-surface is the proven register today.
- **A separate `challenge` CLI.** The per-project suite binary is the runner; a wrapper binary is scope without a consumer.
- **Watch / interactive modes.** Checkpoint navigation covers the debugging loop; anything fancier waits for demonstrated need.

## Non-Goals

- **YAML or any document front-end.** Considered and dropped in the design session: capture is interwoven with flow and cannot be pushed to the edges, so a schema becomes a versioned public contract growing toward a bad programming language. Interpolated command literals close most of the readability gap; the transcript supplies the document-shaped artifact from ground truth.
- **Parallel challenge execution.** The world is singular by design; parallelism fights the point.
- **Non-Go systems.** challenge is for the Go-based, df-shaped portfolio. The plugins have their own testing culture.
- **Browser/UI testing.** SPA coverage stays at serves-the-shell smoke; real UI testing is a different tool.
