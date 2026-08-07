// Command toy is a small df-shaped product for the challenge library to press
// against: it emits dl messages, writes dd-marshaled state, serves a trivial wire
// surface, and can be told to misbehave in each of the specific ways the harness
// claims to detect.
//
// it exists so the engine is proven against real subprocesses — real exit codes,
// real stdin, a real socket, real signals — rather than against a mock of them.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/michaelquigley/df/dd"
	"github.com/michaelquigley/df/dl"
	"golang.org/x/sys/unix"
)

// state is the toy's persisted shape, and the body its wire surface answers with.
type state struct {
	Id    string
	Label string
}

// drifted is the same state after a rename on the shipped surface, for pressing
// on what a mirror does when the product's format moves.
type drifted struct {
	Identifier string
	Label      string
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: toy <command> [args]")
	}
	command, args := os.Args[1], os.Args[2:]

	// the error path renders to stderr, the way a df-shaped CLI does.
	if command == "fail" || command == "prompt" {
		dl.Init(dl.DefaultOptions().SetOutput(os.Stderr))
	} else {
		dl.Init()
	}

	switch command {
	case "emit":
		dl.Infof("%s", strings.Join(args, " "))
	case "fail":
		dl.Errorf("%s", strings.Join(args, " "))
		os.Exit(1)
	case "twice":
		msg := strings.Join(args, " ")
		dl.Errorf("%s", msg)
		dl.Errorf("%s", msg)
		os.Exit(1)
	case "digest":
		dl.Infof("snapshot 9f2c1ab77e40 captured")
	case "digests":
		dl.Infof("snapshot 9f2c1ab77e40 captured")
		dl.Infof("snapshot 3b81de0044c5 captured")
	case "split-marker":
		// half a crash marker on each stream, adjacent to nothing.
		fmt.Fprint(os.Stdout, "pan")
		fmt.Fprintln(os.Stderr, "ic: not really")
	case "table":
		// raw stdout that is not a dl message, with the variable cell padding a
		// table renderer produces.
		fmt.Println("name        size     status")
		fmt.Println("personal    70MB     satisfied")
	case "args":
		for _, a := range args {
			dl.Infof("arg %s", a)
		}
	case "env":
		if len(args) != 1 {
			fail("usage: toy env <key>")
		}
		dl.Infof("%s=%s", args[0], os.Getenv(args[0]))
	case "state":
		writeState(args)
	case "panic":
		panic("the toy came apart")
	case "daemon":
		// backgrounds a child and exits, leaving a writer nothing declared and
		// nothing supervises.
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			fail("opening %s: %v", os.DevNull, err)
		}
		child := exec.Command(os.Args[0], "sleep", "--ignore-term", "60s")
		child.Stdin, child.Stdout, child.Stderr = devnull, devnull, devnull
		if err := child.Start(); err != nil {
			fail("spawning a child: %v", err)
		}
		dl.Infof("left %d running", child.Process.Pid)
		if len(args) == 1 && args[0] == "--panic" {
			// comes apart *and* leaves something behind: two things to say about
			// one invocation, and only one of them is about the product.
			panic("the toy came apart after leaving a child")
		}
	case "kill":
		// death by signal with no marker and no output: the evidence is the
		// manner of the exit and nothing else.
		_ = unix.Kill(os.Getpid(), unix.SIGKILL)
	case "prompt":
		prompt()
	case "sleep":
		sleep(args)
	case "serve":
		serve(args)
	default:
		fail("unknown command %q", command)
	}
}

// writeState records a dd-marshaled state file under the working directory.
func writeState(args []string) {
	if len(args) != 3 {
		fail("usage: toy state <path> <id> <label>")
	}
	if err := dd.UnbindYAMLFile(&state{Id: args[1], Label: args[2]}, args[0]); err != nil {
		fail("writing %s: %v", args[0], err)
	}
	dl.Infof("wrote state to %s", args[0])
}

// prompt reads a real answer from real stdin, because how a prompt behaves at the
// shell is product behavior.
func prompt() {
	fmt.Print("proceed? [y/N] ")
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		answer = ""
	}
	if !strings.EqualFold(strings.TrimSpace(answer), "y") {
		dl.Errorf("declined")
		os.Exit(1)
	}
	dl.Infof("proceeding")
}

// sleep waits, so a caller can decide how the process ends. --ignore-term makes it
// outlive a group signal, which is how a descendant that quiescence has to notice
// gets built.
func sleep(args []string) {
	d := time.Second
	for _, a := range args {
		if a == "--ignore-term" {
			signal.Ignore(unix.SIGTERM, unix.SIGINT)
			continue
		}
		parsed, err := time.ParseDuration(a)
		if err != nil {
			fail("bad duration %q: %v", a, err)
		}
		d = parsed
	}
	dl.Infof("sleeping for %s", d)
	time.Sleep(d)
}

// serveOptions is how a supervised toy can be told to misbehave.
type serveOptions struct {
	port        string
	neverReady  bool
	slowStop    time.Duration
	dieAfter    time.Duration
	panicAfter  time.Duration
	panicOnStop bool
	exitOnStop  int
	closeAfter  time.Duration
	markerAfter time.Duration
	refuseIf    string
	orphan      bool
	drift       bool
}

// serve runs the toy's wire surface.
func serve(args []string) {
	opts := parseServeOptions(args)

	if opts.refuseIf != "" {
		if _, err := os.Stat(opts.refuseIf); err == nil {
			// refuses to come up at all, so a boundary restart can be pressed on
			// without the first start having to fail too.
			fail("refusing to start while %s exists", opts.refuseIf)
		}
	}

	if opts.orphan {
		// a process the fixture started, sharing its group and outliving a group
		// SIGTERM: the writer quiescence has to notice before a snapshot is taken.
		child := exec.Command(os.Args[0], "sleep", "--ignore-term", "120s")
		if err := child.Start(); err != nil {
			fail("spawning a child: %v", err)
		}
		dl.Infof("spawned child %d", child.Process.Pid)
	}
	if opts.exitOnStop != 0 {
		// a shutdown path entitled to return whatever it likes. asked to stop, it
		// stops — the status it chooses is not a crash.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, unix.SIGTERM, unix.SIGINT)
		go func() {
			<-ch
			dl.Infof("stopping with status %d", opts.exitOnStop)
			os.Exit(opts.exitOnStop)
		}()
	}
	if opts.panicOnStop {
		// coming apart in the shutdown path is a product failure whoever asked the
		// process to stop.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, unix.SIGTERM, unix.SIGINT)
		go func() {
			<-ch
			panic("the toy came apart on the way down")
		}()
	}
	if opts.slowStop > 0 {
		// a process that will not close cleanly is the case quiescence has to be
		// able to call a harness fault rather than absorb.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, unix.SIGTERM, unix.SIGINT)
		go func() {
			<-ch
			dl.Infof("stopping slowly")
			time.Sleep(opts.slowStop)
			os.Exit(0)
		}()
	}
	if opts.dieAfter > 0 {
		go func() {
			time.Sleep(opts.dieAfter)
			// no marker, no signal: the death evidence is the exit itself.
			os.Exit(7)
		}()
	}
	if opts.markerAfter > 0 {
		// prints what a Go process coming apart looks like, without dying: evidence
		// in the window of a fixture that is still there to refuse to stop.
		go func() {
			time.Sleep(opts.markerAfter)
			fmt.Println("panic: the toy is unwell")
		}()
	}
	if opts.panicAfter > 0 {
		go func() {
			time.Sleep(opts.panicAfter)
			panic("the toy server came apart")
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/config", func(res http.ResponseWriter, _ *http.Request) {
		if opts.neverReady {
			// live, answering, and never usable.
			res.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(res, `{"status":"starting"}`)
			return
		}
		fmt.Fprintf(res, `{"name":"toy","port":%s}`, opts.port)
	})
	mux.HandleFunc("/api/v1/state", func(res http.ResponseWriter, _ *http.Request) {
		var body []byte
		var err error
		if opts.drift {
			body, err = dd.UnbindJSON(&drifted{Identifier: "toy-1", Label: "personal"})
		} else {
			body, err = dd.UnbindJSON(&state{Id: "toy-1", Label: "personal"})
		}
		if err != nil {
			res.WriteHeader(http.StatusInternalServerError)
			return
		}
		res.Header().Set("Content-Type", "application/json")
		res.Write(body)
	})
	mux.HandleFunc("/api/v1/broken", func(res http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(res, "this is not a document any mirror will take")
	})
	mux.HandleFunc("/api/v1/missing", func(res http.ResponseWriter, _ *http.Request) {
		res.WriteHeader(http.StatusNotFound)
		fmt.Fprint(res, `{"error":"no such thing"}`)
	})
	mux.HandleFunc("/api/v1/jobs", func(res http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			res.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		res.WriteHeader(http.StatusCreated)
		fmt.Fprint(res, `{"id":"job-1","state":"running"}`)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:"+opts.port)
	if err != nil {
		fail("listening: %v", err)
	}
	srv := &http.Server{Handler: mux}
	if opts.closeAfter > 0 {
		// stops answering while staying alive: a wire that refuses without the
		// process behind it having died.
		go func() {
			time.Sleep(opts.closeAfter)
			dl.Infof("closing the listener")
			srv.Close()
		}()
	}
	dl.Infof("serving on port %s", opts.port)
	go func() {
		_ = srv.Serve(ln)
	}()
	// the process outlives its listener, so closing one is not the same as dying.
	select {}
}

// parseServeOptions reads the serve flags by hand, so the toy stays a single file
// with no dependencies beyond the house libraries.
func parseServeOptions(args []string) serveOptions {
	opts := serveOptions{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			i++
			if i >= len(args) {
				fail("--port wants a value")
			}
			opts.port = args[i]
		case "--never-ready":
			opts.neverReady = true
		case "--panic-on-stop":
			opts.panicOnStop = true
		case "--orphan":
			opts.orphan = true
		case "--exit-on-stop":
			i++
			if i >= len(args) {
				fail("--exit-on-stop wants a value")
			}
			code, err := strconv.Atoi(args[i])
			if err != nil {
				fail("bad exit status %q: %v", args[i], err)
			}
			opts.exitOnStop = code
		case "--drift":
			opts.drift = true
		case "--slow-stop":
			i++
			opts.slowStop = mustDuration(args, i)
		case "--die-after":
			i++
			opts.dieAfter = mustDuration(args, i)
		case "--panic-after":
			i++
			opts.panicAfter = mustDuration(args, i)
		case "--close-after":
			i++
			opts.closeAfter = mustDuration(args, i)
		case "--marker-after":
			i++
			opts.markerAfter = mustDuration(args, i)
		case "--refuse-if":
			i++
			if i >= len(args) {
				fail("--refuse-if wants a path")
			}
			opts.refuseIf = args[i]
		default:
			fail("unknown serve flag %q", args[i])
		}
	}
	if opts.port == "" {
		fail("serve wants --port")
	}
	return opts
}

// mustDuration reads a duration argument or gives up.
func mustDuration(args []string, i int) time.Duration {
	if i >= len(args) {
		fail("a duration flag wants a value")
	}
	d, err := time.ParseDuration(args[i])
	if err != nil {
		fail("bad duration %q: %v", args[i], err)
	}
	return d
}

// fail reports an operational failure the way a df-shaped CLI does: rendered once,
// on stderr, with no stack trace, and a nonzero wire status.
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "toy: "+format+"\n", args...)
	os.Exit(1)
}
