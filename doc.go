// Package challenge is a library for building and maintaining full-system
// pressure suites: long, ordered narratives run end to end against one living
// world, guarding real systems the way a life guards them rather than the way a
// thousand independent unit tests do.
//
// four nouns carry the design.
//
// a world is the singular, living environment a suite runs against: a root
// directory holding the system's workspace, its simulated media, its store —
// everything. the world is just a filesystem, and that fact is load-bearing,
// because it makes checkpointing, inspection, and cleanup ordinary file
// operations. the world also supervises the long-lived processes a suite needs.
//
// a challenge is the unit of authorship: a named Go function receiving the world,
// acting through the system's shipped surface, and recording findings. challenges
// are ordered, and each inherits the world its predecessor left behind.
//
// a gauntlet is a project's ordered registry of challenges plus its world
// configuration. it is defined in code, and run whole or navigated by name.
//
// a run is one execution: a run model recording every step, every finding, and
// every verdict as pure data, rendered by separate walkers.
//
// the seam that keeps all of this end-to-end: challenges act only through the
// shipped surface — subprocess and HTTP, real stdin, real exit codes. shared
// libraries are imported for parsing what comes back, never for reaching into the
// system under test to do things.
package challenge
