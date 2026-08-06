# AGENTS.md — challenge

`challenge` is a Go library for building and maintaining full-system pressure suites: long, ordered narratives run end to end against one living world. It is for the df-shaped Go portfolio — CLIs and servers emitting `dl` output and `dd`-marshaled state — and reef is its first consumer.

## The four nouns

Everything in this repo is organized around four words. Use them precisely; they are the design.

**World.** The singular, living environment a suite runs against: a root directory holding the workspace, the simulated media, the store, everything. The world is just a filesystem, and that fact is load-bearing — it makes checkpointing, inspection, and cleanup ordinary file operations. The world also supervises the long-lived processes a suite needs.

**Challenge.** The unit of authorship: a named Go function receiving the world, acting through the system's shipped surface, and recording findings. Challenges are ordered, and each inherits the world its predecessor left behind.

**Gauntlet.** A project's ordered registry of challenges plus its world configuration. Defined in code, run whole or navigated by name.

**Run.** One execution: a run model recording every step, every finding, and every verdict as pure data, rendered by separate walkers.

## The seam

Challenges act only through the shipped surface — subprocess and HTTP, real stdin, real exit codes. `dd` and `dl` are imported for *parsing* what comes back, never for reaching into the system under test to *do* things. A suite package imports nothing from the product tree beyond its own vocabulary package, and the vocabulary is under the same rule so it cannot launder access.

## The error census

Four classes, never blurred, because a guardian suite's worst failure is a verdict that lies:

- **harness fault** — the harness or the suite is broken. run invalid, exit 2.
- **crash** — an evidential product death: a panic marker, an unsolicited signal, a supervised process found dead. terminal, exit 1.
- **break** — a product-surface failure severing dependent flow: a failed capture, a decode mismatch, a refused wire, a fixture that never became ready. terminal, exit 1, below crash.
- **assertion** — a counted finding. the corridor continues; exit 1.

Assertion is the only non-terminal class. A new failure mode is assigned to an existing class; the census is re-opened deliberately in design, never ad hoc in code.

## Conventions

Go code follows the house style: `dl` for logging, `dd` for marshaling, lowercase comments and output, `camelCase` file names after the primary type, `Id` rather than `ID`.

`CHANGELOG.md` follows the in-house format: newest-first releases, prose entries led by `FEATURE`/`CHANGE`/`FIX`, and an `## Unreleased` slot new entries go into.

Before presenting any markdown you authored or edited, run `unfurl -i <file>` on it.

## Project memory

Durable knowledge about this project lives in `docs/journal/`, dated files `docs/journal/YYYY-MM-DD.md`. This is project memory; it does not go in harness-local storage (`.claude/` or equivalent), where it's invisible to every other harness and collaborator and dies with the host. Concretely: do not write to your harness's memory directory or memory tool for this project — even when the harness presents it as the default place for durable knowledge. That tool is the silo this convention exists to replace; the journal is the only durable home.

On arrival, read the most recent entries to pick up where the last session left off, before you start changing things. Treat them as prior-session context, not verified truth — if an entry conflicts with the code or a `docs/current/` doc, the code wins.

Write the smallest entry that carries the session's durable insight, and nothing more. The test for every line: *would a competent agent get this wrong, or waste time rediscovering it, working from the tree alone?* If it's recoverable by reading the code, the diff, `docs/current/`, or git history, leave it out.

That filter keeps four kinds of thing and discards the rest:

- **Decisions whose rationale isn't visible in the result** — why a value was chosen, what a line guards against, why something that looks like dead code or a no-op is load-bearing.
- **Deliberate non-actions** — a change you considered and chose not to make, so the next agent doesn't "fix" it. An unchanged file leaves no trace in a diff.
- **Couplings that span files** — two places that must move together, an ordering that matters, an assumption one file makes about another.
- **Live state** — what's unverified, unfinished, or waiting on something external.

Skip change inventories, restatements of the diff, and play-by-play of how you worked. There's no write-time approval gate; Michael reviews on commit. Append to the day's file if it exists, and write the few lines you'd want the next agent to read — honest and self-contained.

## Documentation

`docs/current/` describes what is built. `docs/future/` holds the intent layer — the spec and work order this library is being built from — and its documents are removed once realized.
