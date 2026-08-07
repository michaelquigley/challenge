package challenge

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// Main is the standalone face: a project's suite is its own binary, consumable by
// make, cron, and CI without the go test machinery in the way.
//
// it is a walker over the run model and holds no logic of its own. exit codes
// carry the verdict in the same wire-status philosophy the products themselves
// use: 0 clean, 1 findings, 2 harness fault.
func Main(g Gauntlet) {
	os.Exit(MainWith(context.Background(), g, os.Args[1:], os.Stdout, os.Stderr))
}

// MainWith is Main with its edges handed in — the context it cancels on, the
// arguments it parses, and the streams it reports to — so the face itself can be
// exercised rather than only its binary.
func MainWith(ctx context.Context, g Gauntlet, args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet(g.Name, flag.ContinueOnError)
	flags.SetOutput(errOut)
	var (
		from       = flags.String("from", "", "resume from a named challenge, replaying whatever lies between")
		only       = flags.String("only", "", "run one named challenge against its predecessor's boundary")
		list       = flags.Bool("list", false, "print the corridor and exit")
		clean      = flags.Bool("clean", false, "discard the world generation and its residue")
		worldHome  = flags.String("world-home", "", "anchor the world somewhere other than the gauntlet's declared home")
		transcript = flags.String("transcript", "", "write the narrative somewhere other than beside the world")
		verbose    = flags.Bool("verbose", false, "report every step, not only the findings")
	)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	reporter := newConsoleReporter(out)
	if *list {
		if err := g.validate(); err != nil {
			fmt.Fprintf(errOut, "%v\n", err)
			return 2
		}
		reporter.list(g)
		return 0
	}

	// the face owns interruption. children run in their own process groups, so a
	// keyboard interrupt reaches only the harness: provenance is recorded first, the
	// engine's unwind stops everything it started, and the run says it is invalid
	// rather than leaving a live writer behind a released lock.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, unix.SIGINT, unix.SIGTERM)
	defer signal.Stop(signals)

	go func() {
		select {
		case <-signals:
			cancel()
		case <-ctx.Done():
		}
	}()

	run := Execute(ctx, g, Options{
		From:       *from,
		Only:       *only,
		Clean:      *clean,
		WorldHome:  *worldHome,
		Transcript: *transcript,
		Verbose:    *verbose,
		Report:     reporter,
	})
	reporter.summary(run)
	return run.Verdict()
}
