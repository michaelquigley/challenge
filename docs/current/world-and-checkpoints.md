# the world, its lifecycle, and the save-point model

What is built today: the world tree and its lifecycle, the four-class error census, the run model, the world authoring surface, and the checkpoint save-point model. Process supervision, the command and HTTP channels, the engine, the renderers, and the two faces land in later increments.

## the tree

A gauntlet's world lives at a stable, declared path — `<worldHome>/.challenge/<gauntlet>/`. The home is an absolute path the gauntlet carries, never something discovered from the working directory: products record absolute paths inside their own state (reef's store does, from the first `container init`), so a checkpoint restored anywhere but its original root is dead on arrival, and standalone, `make`, and `go test`'s temporary binary all have to land on one world.

```
<worldHome>/.challenge/
    <gauntlet>.lock        the exclusive guard, anchored beside the tree
    <gauntlet>/
        world/             the checkpointed root: everything the product owns
            .harness/      deposits and world environment — inside the image
        bin/               build artifacts; never checkpointed
        checkpoints/       NN-name/ save points, one per boundary
        logs/              supervised-process output
        transcript.md      the run's readable narrative
        session.yaml       the world generation and its corridor shape
        run.yaml           the run pointer and the fail-closed prune record
```

The split is by what the state describes. State describing the world *at a boundary* — the deposits, the world environment, the process registry — lives inside `world/.harness/` and therefore inside the checkpoint image, so it rolls back with every restore. State describing the *session* — the lock, the run pointer, the logs, the transcript — stays outside and survives it.

The lock is a *sibling* of the gauntlet directory, not a child. Unlinking a locked file leaves the old inode locked while a second process creates and locks a fresh one at the same path, so a guard living inside the thing it guards is defeated by exactly the destructive operations — reset, restore, clean — it exists to make safe. Every lifecycle operation acquires the lock first and mutates only the tree's children.

## lifecycle

The world keeps one generation of history. A fresh full run resets the tree and mints a session identity; a failed or aborted run leaves the world, its checkpoints, and the partial transcript in place, and that residue *is* the debugging session a resume picks up. `clean` discards it explicitly, emptying the tree's children and never touching the lock.

Reset spares `bin/`. The bootstrap that produced the binary under test has already run by then, and the artifact it built is not world state — it lives beside the world so no checkpoint ever contains a binary, and resume therefore presses current code against restored state.

Reset also publishes the fresh world as boundary zero. Genesis belongs to reset rather than to whatever sequences it: boundary zero is what makes the corridor's first challenge resolve like any other, and a world generation that could exist without its own boundary zero is a coordinate system with a hole in it.

## the error census

Every failure the harness can observe belongs to exactly one of four classes, and the class governs both the verdict and the control flow that follows it.

| class | what it means | control flow | verdict |
|---|---|---|---|
| harness fault | the harness or the suite itself is broken — an invalid input, unreadable harness-owned state, a spawn or cleanup failure | terminal; run invalid | exit 2 |
| crash | an evidential product death — a panic marker, an unsolicited signal, a supervised process found dead | terminal | exit 1 |
| break | a product-surface failure severing dependent flow — a failed capture, a decode mismatch, a refused wire, a fixture that never became ready | terminal | exit 1 |
| assertion | a counted finding: a wording or value mismatch severing nothing | the corridor continues | exit 1 |

Assertion is the only non-terminal class. Terminal findings end the invocation by panicking a private sentinel the engine recovers — a challenge is ordinary Go, and leaving the body is exactly what "terminal" means. The alternative, letting a failed capture or an undecodable body return a zero value, is how a break ripens into a confusing failure three steps downstream wearing the wrong class.

The verdict is computed from the run model alone, so every renderer reports the same one without deriving a verdict of its own.

## the authoring surface, so far

`Dir` returns an absolute path under the world and creates the directory; `Path` names a location without making one, and passes an already-absolute path straight through. Both require the result to stay inside the world: outside it lie the harness's own session state and the checkpoint images, so a suite path that climbs out is a harness fault rather than a write that corrupts the world the verdict is about. `BinDir` is where artifacts go.

Containment is checked at two depths, because observing and writing are not the same risk. Every path is checked lexically, so traversal out of the world faults. A path about to be *written* is checked again physically: its deepest existing ancestor is resolved, and a path that reaches outside through a symlink faults before anything is created. The world under test is full of links the product put there, and a `world/media` pointing elsewhere makes `media/config.yaml` lexically innocent and physically outside. Reads stay lexical — an assertion about a symlink pointing anywhere it likes is a legitimate thing for a challenge to make.

A gauntlet's name becomes a directory and a lock file, so it is validated rather than normalized — a plain path component of letters, digits, and `.-_`. Sanitizing would let two differently-named gauntlets share one world in silence, and a name like `..` would resolve the tree somewhere the lifecycle operations have no business touching.

`Put` and `Get` are the deposit store. A value held in a local is challenge-local by nature — resume starts execution partway down the corridor, and a variable an earlier challenge assigned was never assigned in a resumed process — so a value a *later* challenge needs is deposited into the world instead. The deposit is a file under the world root, so it rides the checkpoint and rolls back with it. `Setenv` deposits an environment fact the same way and for the same reason. A `Get` of a deposit that never happened is a harness fault, not a finding.

`FreePort` binds and releases a port so the vocabulary can write a collision-free bind into the world's config.

`WriteFile` is a harness action — setup, fault injection, a config the vocabulary authors — so a failure is a harness fault. `ReadFile`, `ReadYAML`, and `ReadJSON` read product-owned bytes whose value travels onward, so bytes that are not there, or that will not fit the mirror, are a break.

`ReadYAML` and `ReadJSON` bind with `dd`'s forgiving default, because a mirror is deliberately narrower than the state file it reads and a strict posture would reject every product field the mirror does not care about. What makes drift signal is the mirror's own declaration: a field the suite depends on carries `dd:"+required"`, so a rename or a removal on the shipped surface fails the bind rather than quietly yielding a zero value.

`Exists`, `Absent`, and `SameBytes` are the filesystem assertions. Absence is only ever `ENOENT`: any other error means the harness could not tell, and reading that as "not there" is how a suite passes on a world it never saw. `SameBytes` streams its comparison, so a 70MB round-trip costs no more memory than a small one, and it reports a missing path as missing rather than folding absence into inequality.

## the copy contract

A checkpoint is a faithful copy of a closed world. The contract covers more than bytes:

- regular-file contents
- file and directory mode bits
- modification times, with directory times applied after their children are written, since writing a child re-stamps its parent
- empty directories
- symlinks preserved as links, never dereferenced, carrying their own modification times rather than stamping what they point at

Any other node type — socket, device, fifo — is a harness fault rather than a silent skip. A snapshot that cannot be faithful refuses to exist.

Modes and times land in a second, deepest-first pass. A directory restored to a read-only mode before its children are written cannot receive them, and reef's dying-drive drill flips a directory to `0555` for real, so the ordering is load-bearing rather than tidy. For the same reason, removing a tree opens its directory modes as it descends: a world the product made read-only is still a world the harness has to be able to discard.

Copies are reflink-first — a per-file `FICLONE` with transparent fallback to a full copy, `--reflink=auto` semantics in-process with no external `cp` dependency. None of this practice's operating environments run a reflink-capable filesystem, so the full copy is the working cost everywhere; the attempt is kept because it is free, not because there is a plan to collect on it.

Publication is atomic. A checkpoint builds under a temporary sibling and renames into its canonical name only when the copy completes, so a failed or interrupted copy is never selectable by the resume resolver. A boundary being republished is retired by rename rather than deleted in place: deleting first would let a failure partway through leave a half-erased image standing at the canonical name, where the resolver would find it and restore a world that was never saved. Every intermediate state is therefore either the old complete image or the new one.

Anything in the checkpoint directory that is not a save point and not a build in progress is corrupted harness-owned state, and listing refuses rather than skipping it — quietly skipping would let navigation fall back to an earlier boundary instead of saying the world cannot be trusted.

## the save-point coordinates

Checkpoints are *post-challenge boundaries*. Boundary N is the world after challenge N; boundary zero is genesis, the fresh world snapshotted at reset so the corridor's first challenge resolves like any other rather than being a special case.

Resuming challenge N restores the greatest boundary **strictly before** N, replays the intervening challenges live, and executes N against its predecessor's world — never against its own aftermath. Where a challenge has opted out of checkpointing, resume pays in replay rather than in a full re-run.

Running a single challenge is a stricter rule than resuming from one: it needs its *immediate* predecessor's boundary, and nothing else will do, so a missing one refuses plainly rather than silently reaching further back and replaying to get there.

## navigation is one operation

Validation, resolution, restore, prune, and rebase are a single operation that cannot be entered halfway, because each step is only honest in this order:

1. refuse if an earlier navigation never finished
2. validate the corridor prefix — a divergent corridor has to be caught *before* anything moves
3. resolve the boundary under the rule the navigation asked for
4. record the prune intent and flush it
5. restore the world to that boundary
6. remove every save point beyond it
7. rebase the recorded corridor, before replay can publish anything
8. record the navigation complete

Restore and prune are one thing because the instant the world moves back, every save point beyond that boundary describes a timeline that no longer exists, and a later navigation reaching one would report green against the wrong world. Splitting the sequence across call sites would leave both an omitted-call path and a window where a crash strands the world in the past with its abandoned future intact.

The whole sequence is fail-closed. The intent is recorded first and cleared last, so a navigation that fails or is interrupted anywhere in between leaves the record set — and the guard sits inside the operation, so a second attempt cannot walk past it into one of the checkpoints that survived. `clean` is the recorded way forward.

## the session record

A reset mints a *session identity* — the world generation — and records the corridor's shape beside it: the ordered challenge names and their checkpoint policies. Every invocation mints a *run identity* of its own, stamped through the run model and the checkpoints it publishes.

Navigation validates before it restores. The current gauntlet's prefix through the requested target must match the recorded prefix exactly; a challenge inserted, removed, renamed, or reordered since the residue was made is a harness fault naming the session and the divergence point, with `clean` as the way forward.

A changed *future* is a different thing from a changed past. When the prefix through the target matches but the suffix has moved, the resume is valid and branches — and the session rebases, its recorded shape replaced with the current gauntlet's after the frontier clears the abandoned future and before replay publishes anything, so record and world move together. Suites are meant to evolve: checkpoints from a corridor that no longer exists must refuse, and checkpoints from the corridor that now exists must resolve.

Names govern divergence; a prefix challenge's checkpoint *policy* does not. The recorded policies describe which boundaries a corridor was expected to produce, and the rebase keeps them current — but flipping one changes nothing about the world a surviving boundary holds. Boundary N stays a truthful record of the world after challenge N whichever way the policy later moved, and restoring a prefix challenge's earlier output is what resume does by design. Refusing there would discard a whole world generation over an authoring tweak that made no snapshot untrue.

## known limits

Harness-allocated ports are written into product config *inside* the world by the consumer's vocabulary, so a checkpoint carries them and a resumed run's fixture restart re-reads the old bind. A collision on the resume-day host surfaces as a fixture that never becomes ready. Re-allocating at restore time would require the harness to know which files are product config, which is exactly the domain knowledge the core/vocabulary split keeps out of core — so for now the behavior is documented and `clean` is the reset.

The posture is linux-first: `FICLONE` and process-group signal handling are the linux-specific pieces. The fallback copy keeps other unix hosts functional but unproven.
