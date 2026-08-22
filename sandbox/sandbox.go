// Package sandbox is the build-sandbox seam (FACTORY.md §4). Container, nix and
// plain-subprocess execution all reach the loop through one interface, and — the
// point of the seam — through one implementation: an Exec whose Wrapper turns a
// subprocess into a container run or a nix shell. There is no Container type
// with its own code path, because a tier or an isolation mode that needs a
// branch here is the abstraction having failed (§1.2).
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/varvig/varvig-factory/cell"
)

// ErrIndescribable is returned by a sandbox that cannot report a reproducible
// environment fragment. Same rule as the model runtime: an adapter that cannot
// describe itself cannot participate in cross-cell selection (§4).
var ErrIndescribable = errors.New("sandbox: adapter cannot describe its environment reproducibly")

// Job is one check to run.
//
// A Job is a command and a working directory. It carries no read set, no write
// set and no ordering, because assigning those would make this a scheduler
// (CELL.md §10.1) — the directory it runs in is the sparse checkout varvig
// already materialized for the task's scope.
type Job struct {
	// Name is the check name that lands in the evidence record.
	Name string
	// Dir is the working directory: the task's checkout.
	Dir string
	// Command is argv. It is not a shell string: a shell string would make the
	// sandbox's isolation depend on which shell the host happens to have, which
	// is precisely git's hooks failure that varvig's wasm hooks exist to avoid.
	Command []string
	// Env are extra environment variables.
	Env []string
	// Timeout bounds the run. Zero means the sandbox's default.
	Timeout time.Duration
}

// Result is one check's outcome, in the shape evidence needs.
type Result struct {
	Name       string
	Status     cell.Status
	DurationMS int64
	// Output is the tail of combined stdout/stderr, for a human reading a
	// failure. Never parsed for a verdict — Status is the verdict.
	Output string
}

// Sandbox is the build-sandbox seam.
type Sandbox interface {
	Name() string
	// Fragment reports the platform, toolchain versions and outcome-affecting
	// flags of the environment jobs will actually run in (CELL.md §4.2, §6).
	Fragment(ctx context.Context) (cell.Fragment, error)
	// Run executes one job. A job that exits nonzero is a StatusFail result and
	// a nil error: a failing test is a measurement, not a malfunction. Run
	// returns an error only when the measurement could not be taken at all.
	Run(ctx context.Context, job Job) (Result, error)
}

// Probe measures one toolchain's version by running a command inside the
// sandbox. Measured, not configured: a version read from a config file is a
// claim about the toolchain rather than an observation of it (CELL.md §6).
type Probe struct {
	// Key is the toolchain name in the environment descriptor, e.g. "go".
	Key string
	// Command is argv producing a version on stdout or stderr.
	Command []string
	// Extract, if set, reduces the raw first line to the token recorded. Use it
	// to drop build details that embed a path or a timestamp — those would make
	// two machines of genuinely the same class hash differently.
	Extract func(string) string
}

// Exec is the one sandbox implementation. Its Wrapper is what makes it three
// adapters:
//
//	plain subprocess: Wrapper nil
//	container:        Wrapper {"docker","run","--rm","-v","{{dir}}:/w","-w","/w","<image>@<digest>"}
//	nix:              Wrapper {"nix","develop","--ignore-environment",".#ci","-c"}
//
// The wrapper is prepended to every command, probes included — so the versions
// recorded in the environment are the versions inside the sandbox, not the
// host's. Getting that backwards is the subtle bug this design forecloses: a
// container cell would otherwise publish its host's Go version and compare as
// same-class with a cell that has no container at all.
type Exec struct {
	// Wrapper is prepended to every command. The literal token "{{dir}}" in any
	// element is replaced with the job's directory, so a container mount can
	// name it.
	Wrapper []string
	// Probes measure the toolchains this sandbox exposes.
	Probes []Probe
	// Platform overrides the reported platform. Empty means the host's
	// GOOS/GOARCH, which is right for a subprocess and for a container running
	// natively; set it explicitly when the sandbox emulates another platform,
	// since reporting the host's would make two genuinely different grounds
	// compare as one.
	Platform string
	// Flags are outcome-affecting build/test flags recorded in the environment.
	// They are configuration by nature — they are the operator's choices — but
	// they are recorded so that a cell running with CGO_ENABLED=1 is visibly
	// not the same ground as one running without.
	Flags map[string]string
	// Container is the artifact content hash of the image this sandbox runs in,
	// if any. It is recorded in the environment so the image is part of the
	// comparison, and it must be a digest rather than a tag: a tag is mutable
	// and would make the environment hash stable while the ground moved.
	Container string
	// Env are extra environment variables for every command.
	Env []string
	// DefaultTimeout bounds a job that sets none.
	DefaultTimeout time.Duration
	// Label names this adapter in logs.
	Label string

	once     sync.Once
	fragment cell.Fragment
	fragErr  error
}

// Name implements Sandbox.
func (e *Exec) Name() string {
	if e.Label != "" {
		return e.Label
	}
	if len(e.Wrapper) > 0 {
		return e.Wrapper[0]
	}
	return "subprocess"
}

// Fragment implements Sandbox. It runs every probe once and caches the result:
// probing is what makes the fragment a measurement, caching is what makes it
// deterministic within a run (FACTORY.md §9.4).
func (e *Exec) Fragment(ctx context.Context) (cell.Fragment, error) {
	e.once.Do(func() { e.fragment, e.fragErr = e.probeAll(ctx) })
	return e.fragment, e.fragErr
}

func (e *Exec) probeAll(ctx context.Context) (cell.Fragment, error) {
	platform := e.Platform
	if platform == "" {
		platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	toolchains := map[string]string{}
	// Probes run in a fixed order so that a failure reports the same probe
	// first on every run — a flapping error message over a genuinely broken
	// configuration wastes an operator's afternoon.
	probes := append([]Probe(nil), e.Probes...)
	sort.Slice(probes, func(i, j int) bool { return probes[i].Key < probes[j].Key })
	for _, p := range probes {
		if p.Key == "" || len(p.Command) == 0 {
			return cell.Fragment{}, fmt.Errorf("%w: malformed probe %+v", ErrIndescribable, p)
		}
		out, err := e.capture(ctx, "", p.Command)
		if err != nil {
			return cell.Fragment{}, fmt.Errorf("%w: probe %q: %v", ErrIndescribable, p.Key, err)
		}
		v := firstLine(out)
		if p.Extract != nil {
			v = strings.TrimSpace(p.Extract(v))
		}
		if v == "" {
			return cell.Fragment{}, fmt.Errorf("%w: probe %q produced no version", ErrIndescribable, p.Key)
		}
		toolchains[p.Key] = v
	}
	// A sandbox with no probes describes a platform and nothing else. That is
	// permitted and honest — it just makes every comparison against it turn on
	// the platform alone.
	flags := map[string]string{}
	for k, v := range e.Flags {
		flags[k] = v
	}
	return cell.Fragment{
		Platform:   platform,
		Toolchains: toolchains,
		Flags:      flags,
		Container:  e.Container,
	}, nil
}

// Run implements Sandbox.
func (e *Exec) Run(ctx context.Context, job Job) (Result, error) {
	if len(job.Command) == 0 {
		return Result{}, fmt.Errorf("sandbox: job %q has no command", job.Name)
	}
	timeout := job.Timeout
	if timeout == 0 {
		timeout = e.DefaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	argv := e.wrap(job.Dir, job.Command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// A wrapped command runs wherever the wrapper puts it; an unwrapped one
	// runs in the checkout.
	if len(e.Wrapper) == 0 {
		cmd.Dir = job.Dir
	}
	cmd.Env = append(cmd.Environ(), append(append([]string(nil), e.Env...), job.Env...)...)

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := Result{
		Name:       job.Name,
		DurationMS: elapsed.Milliseconds(),
		Output:     tail(out.String(), 4000),
	}
	switch {
	case err == nil:
		res.Status = cell.StatusPass
	case ctx.Err() != nil:
		// A timeout is StatusError, not StatusFail: the check did not reach a
		// verdict, and recording it as a failure would let a slow machine look
		// like broken code (CELL.md §4.1).
		res.Status = cell.StatusError
		res.Detail(fmt.Sprintf("timed out after %s", timeout))
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.Status = cell.StatusFail
		} else {
			// Could not start at all: a missing binary, a permission problem.
			// That is not the code failing.
			res.Status = cell.StatusError
		}
	}
	return res, nil
}

// Detail appends a short explanation to a result's output.
func (r *Result) Detail(s string) {
	if r.Output == "" {
		r.Output = s
		return
	}
	r.Output = r.Output + "\n" + s
}

// Check converts a Result to the evidence shape.
func (r Result) Check() cell.Check {
	return cell.Check{
		Name:       r.Name,
		Status:     r.Status,
		DurationMS: r.DurationMS,
		Detail:     firstLine(tail(r.Output, 200)),
	}
}

// wrap prepends the wrapper, substituting {{dir}}.
func (e *Exec) wrap(dir string, command []string) []string {
	if len(e.Wrapper) == 0 {
		return command
	}
	argv := make([]string, 0, len(e.Wrapper)+len(command))
	for _, w := range e.Wrapper {
		argv = append(argv, strings.ReplaceAll(w, "{{dir}}", dir))
	}
	return append(argv, command...)
}

// capture runs a command through the wrapper and returns its combined output.
func (e *Exec) capture(ctx context.Context, dir string, command []string) (string, error) {
	argv := e.wrap(dir, command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(e.Wrapper) == 0 && dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(cmd.Environ(), e.Env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, tail(out.String(), 200))
	}
	return out.String(), nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
