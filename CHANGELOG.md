# CHANGELOG

## Unreleased

FEATURE: New `challenge` library for building full-system pressure suites: a project composes named challenges into a gauntlet, an ordered corridor run end to end against one living world. This first landing carries the world tree and its lifecycle, the four-class error census, the run model, and the checkpoint save-point model.

FEATURE: Checkpoints snapshot the world between challenges so a failure deep in a corridor resumes from the boundary before it rather than costing a full re-run. Snapshots are reflink-first honest copies preserving bytes, mode bits, modification times, empty directories, and symlinks; they publish atomically, so a copy that cannot be faithful is never selectable.
