# CHANGELOG

## Unreleased

FEATURE: New `challenge` library for building full-system pressure suites: a project composes named challenges into a gauntlet, an ordered corridor run end to end against one living world. This first landing carries the world tree and its lifecycle, the four-class error census, the run model, and the checkpoint save-point model.

FEATURE: A project composes named challenges into a gauntlet and hands it to a face. The standalone face is a suite binary consumable by `make`, cron, and CI, carrying the verdict in its exit code — 0 clean, 1 findings, 2 harness fault — with `--from`, `--only`, `--list`, and `--clean` for navigating a world between runs. The go-test face maps the same run onto `testing.T` behind a build tag, so `go test ./...` never trips a system suite by accident.

FEATURE: Every run renders a transcript: a markdown narrative of what happened, written as the run happens so a failed one leaves the document that explains it. Resumed runs open a new attempt section rather than appending into the prior narrative, and each attempt's verdict is computed only from its own work.

FEATURE: Challenges act on the world through two channels. Commands are written as the literal a user would type, with `{}` placeholders substituting arguments as whole argv tokens — no shell, so no quoting hazards — and every invocation returns its exit code, its raw streams, and its `dl` messages parsed as data. The wire channel interrogates the same world over HTTP, so a challenge can act through the CLI and verify through the API.

FEATURE: The world supervises long-lived processes. A fixture is declared with a readiness probe, its output captured, and bounced at every challenge boundary — so no snapshot is taken under a live writer, and a resumed run reaches every challenge the same way a full run does.

FEATURE: Checkpoints snapshot the world between challenges so a failure deep in a corridor resumes from the boundary before it rather than costing a full re-run. Snapshots are reflink-first honest copies preserving bytes, mode bits, modification times, empty directories, and symlinks; they publish atomically, so a copy that cannot be faithful is never selectable.
