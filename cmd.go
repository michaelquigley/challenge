package challenge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// waitDelay bounds how long a cancelled command's output pipes are given to drain
// after the harness has signalled it.
const waitDelay = 5 * time.Second

// commandGrace is how long anything left in a finished command's process group is
// given to exit on its own before it counts as a straggler.
const commandGrace = 500 * time.Millisecond

// Cmd is an armed invocation: the literal a user would type, plus the stdin,
// environment, and working directory a particular case needs.
type Cmd struct {
	w       *W
	literal string
	args    []any
	stdin   string
	env     map[string]string
	dir     string
}

// Run executes a command literal immediately and returns its result.
//
// the literal is written the way a user would type it, with {} placeholders
// substituting arguments as whole argv tokens. there is no shell, no splitting of
// substituted values, and therefore no quoting hazard: a token containing spaces
// always rides a placeholder.
func (w *W) Run(literal string, args ...any) *Result {
	return w.Cmd(literal, args...).Run()
}

// Cmd arms an invocation for the cases that need more than the literal — a
// declined prompt reading real stdin, a probe against a foreign workspace.
func (w *W) Cmd(literal string, args ...any) *Cmd {
	return &Cmd{w: w, literal: literal, args: args, env: map[string]string{}}
}

// Stdin gives the invocation something to read. it is the real thing on the real
// descriptor, because how a prompt behaves at the shell is product behavior.
func (c *Cmd) Stdin(s string) *Cmd {
	c.stdin = s
	return c
}

// Env overrides one environment variable for this invocation only.
func (c *Cmd) Env(key, value string) *Cmd {
	c.env[key] = value
	return c
}

// Dir runs the invocation from somewhere other than the world root.
func (c *Cmd) Dir(path string) *Cmd {
	c.dir = path
	return c
}

// Run issues the armed invocation.
//
// a failure to issue at all — a malformed literal, a missing harness-owned binary,
// a spawn error — is a harness fault rather than a product finding, and no
// zero-valued result is manufactured to carry it: the suite never gets a Result
// describing a command that never ran.
func (c *Cmd) Run() *Result {
	w := c.w
	argv := w.resolveArgv(c.literal, c.args)
	bin := w.resolveBinary(argv[0])

	display := strings.Join(argv, " ")
	step := w.step(StepCmd, display, "")
	index := w.stepIndex()

	cmd := exec.CommandContext(w.ctx, bin, argv[1:]...)
	cmd.Dir = w.home.world()
	if c.dir != "" {
		cmd.Dir = c.dir
	}
	cmd.Env = w.childEnv(c.env)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if c.stdin != "" {
		cmd.Stdin = strings.NewReader(c.stdin)
	}
	// every harness-spawned process runs in its own group, isolated from the
	// runner's foreground group, so a keyboard interrupt reaches only the harness
	// and children die by the harness's recorded hand.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// provenance for this command, not for the run. a global "we were interrupted"
	// would erase a genuine crash that happened to land in the same instant, so the
	// flag is set only where the harness's own kill actually reached this process.
	var killed atomic.Bool
	cmd.Cancel = func() error {
		// a signal landing is not proof the harness caused anything: kill succeeds
		// for a process that has already died and not yet been reaped. the probe is
		// what distinguishes stopping a command from arriving after it stopped.
		gone, probeErr := alreadyExited(cmd.Process.Pid)
		err := killGroup(cmd.Process, unix.SIGKILL)
		if err == nil && probeErr == nil && !gone {
			killed.Store(true)
		}
		return err
	}
	cmd.WaitDelay = waitDelay

	started := time.Now()
	err := cmd.Run()
	step.Elapsed = time.Since(started)

	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			w.faultf("issuing %q: %v", display, err)
		}
	}
	// anything left behind is terminated now, but what it *means* waits: a command
	// that came apart and left a child behind is a product crash first and the
	// harness's housekeeping complaint second, and reporting the fault here would
	// hide the crash evidence behind it.
	straggler := clearStragglers(cmd.Process)

	res := &Result{
		w:       w,
		step:    index,
		Literal: display,
		Exit:    cmd.ProcessState.ExitCode(),
		Stdout:  out.String(),
		Stderr:  errOut.String(),
	}
	res.Msgs = append(parseMsgs(res.Stdout), parseMsgs(res.Stderr)...)
	step.Exit = res.Exit
	step.Detail = strings.TrimSpace(strings.Join(res.Msgs, "; "))

	// classification comes before the interruption leaves, so a command that came
	// apart of its own accord in the same instant keeps the finding it earned.
	res.detectCrash(cmd.ProcessState, killed.Load())
	if w.interrupted() {
		w.abandon()
	}
	if straggler {
		w.faultf("%q left processes running after it exited; a world nothing is supervising cannot be snapshotted honestly", display)
	}
	w.pending = append(w.pending, res)
	return res
}

// clearStragglers terminates anything a finished command left running and reports
// whether there was anything to terminate.
//
// a product that backgrounds a child and exits leaves a writer nothing is
// supervising: it is not a declared fixture, so quiescence never reaches it, and a
// boundary snapshot taken while it works would copy a world still being changed. a
// suite that wants a long-lived process declares one.
func clearStragglers(p *os.Process) bool {
	if p == nil || groupGone(p.Pid) {
		return false
	}
	// anything mid-exit gets a moment before it counts as a straggler.
	deadline := time.Now().Add(commandGrace)
	for time.Now().Before(deadline) {
		if groupGone(p.Pid) {
			return false
		}
		time.Sleep(readyInterval)
	}
	_ = clearGroup(p.Pid, reapGrace)
	return true
}

// resolveArgv splits a literal on whitespace and substitutes its placeholders. a
// literal the harness cannot turn into an argv is a broken harness input.
func (w *W) resolveArgv(literal string, args []any) []string {
	fields := strings.Fields(literal)
	if len(fields) == 0 {
		w.faultf("command literal is empty")
	}
	argv := make([]string, 0, len(fields))
	used := 0
	for _, f := range fields {
		if f == "{}" {
			if used >= len(args) {
				w.faultf("literal %q wants more arguments than the %d given", literal, len(args))
			}
			argv = append(argv, fmt.Sprint(args[used]))
			used++
			continue
		}
		if strings.Contains(f, "{}") {
			w.faultf("literal %q embeds a placeholder inside the token %q; a placeholder is a whole argv token", literal, f)
		}
		argv = append(argv, f)
	}
	if used != len(args) {
		w.faultf("literal %q has %d placeholders for %d arguments", literal, used, len(args))
	}
	return argv
}

// resolveBinary finds the program a literal names, in the world-adjacent bin/ the
// bootstrap builds into.
//
// there is deliberately no PATH fallback. the products these suites guard are
// usually installed on the machines that test them, so a fallback would let a
// bootstrap that quietly failed to produce a binary be papered over by whatever
// version happens to be on PATH — a green verdict about a build nobody made. a
// name with no separator resolves in bin/ or not at all; a vocabulary that needs a
// system tool names its path.
func (w *W) resolveBinary(name string) string {
	if strings.ContainsRune(name, filepath.Separator) {
		if _, err := os.Stat(name); err != nil {
			w.faultf("no binary at %s: %v", name, err)
		}
		return name
	}
	candidate := filepath.Join(w.home.bin(), name)
	info, err := os.Stat(candidate)
	if err != nil {
		w.faultf("no binary named %q in %s: the bootstrap is what puts it there", name, w.home.bin())
	}
	if info.IsDir() {
		w.faultf("%s is a directory, not the %q binary", candidate, name)
	}
	return candidate
}

// childEnv assembles the environment a child sees: the harness's own, then the
// world's deposited facts, then this invocation's overrides.
//
// nothing about the rendering is forced. a harness subprocess is always piped, and
// dl selects its JSON transport from that fact alone — which is the surface cron
// and CI live on, so it is shipped surface in its own right. forcing an override
// would pass the suite while quietly retiring dl's own default-selection from test.
func (w *W) childEnv(overrides map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for k, v := range w.env {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// killGroup signals a process's whole group. every harness-spawned process runs in
// its own group, so the signal reaches the process and anything it started, and
// nothing else.
func killGroup(p *os.Process, sig unix.Signal) error {
	if p == nil {
		return nil
	}
	return unix.Kill(-p.Pid, sig)
}
