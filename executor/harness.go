package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/varvig/varvig-factory/cell"
)

// Harness drives an agent harness — Claude Code, or anything shaped like it —
// as a subprocess working inside the task checkout.
//
// This is the second row of FACTORY.md §4.5's wiring table, and until now it
// had no implementation: everything else in this package runs in the cell's
// process, answers once, and returns text. A harness does none of those things.
// It gets a directory, edits files in it, loops on what it sees, and talks to
// varvig over a socket while it works.
//
// # It needed no new Factory concept, which was the whole bet
//
// The fold (§4) claimed that a new executor should require no new interface, no
// new package and no new config kind. This is the first executor written to
// test that claim rather than to illustrate it, and the claim held with two
// qualifications, both of them additions to a table rather than new concepts:
//
//   - Request gained Dir and Socket, because the wiring an executor gets is
//     proportional to the properties it declares, and nothing before this
//     declared these.
//   - ExecutorProperties gained EditsInPlace, because "my response is a report"
//     and "my response is content to apply" are different contracts and a
//     caller cannot guess which it holds.
//
// Neither is a case in a switch. The loop reads properties; it still cannot ask
// what an executor *is*, and the guard in guard/ still fails the build on any
// attempt to.
//
// # What it is not
//
// Not a sandbox. The subprocess runs with this cell's user and this cell's
// filesystem access, confined to the checkout only by convention and by its own
// behaviour. The confinement that is real is on the **tool channel**: the
// socket is scoped and propose-only, so whatever the harness asks varvig to do
// is bounded by what varvig already granted the task. That is the §4.2 line —
// authority attaches to the tool channel, never to the model — and it is worth
// being precise about, because "it runs in a checkout" sounds like isolation
// and is not.
type Harness struct {
	// Path is the harness executable.
	Path string
	// Args are passed to every invocation, before PromptArgs.
	Args []string
	// PromptArg names the flag the intent is passed with, e.g. "-p". Empty
	// sends the intent on stdin instead, which is the safer default: an intent
	// is arbitrary text and argv has limits a spec can exceed.
	PromptArg string
	// VersionArgs produce a version string on stdout or stderr, e.g.
	// {"--version"}. REQUIRED: without it the harness cannot describe itself,
	// and an executor that cannot do that cannot take part in cross-cell
	// selection at all.
	VersionArgs []string
	// Model and ModelVersion identify what the harness runs, recorded in the
	// environment. A harness usually knows its model better than its operator
	// does, but it is not asked: a measured claim from the thing being measured
	// is the one number an environment descriptor must not take on trust.
	Model, ModelVersion string
	// SocketEnv is the environment variable the task MCP socket is passed in,
	// e.g. "VARVIG_MCP_SOCKET". Empty means the socket is not passed at all,
	// which is the right configuration for a harness that takes no tools.
	SocketEnv string
	// Env are extra environment variables for the process.
	Env []string

	once     sync.Once
	fragment cell.Fragment
	fragErr  error
}

// Name implements cell.Executor.
func (h *Harness) Name() string { return "harness" }

// Properties implements cell.Executor.
//
// Deterministic is false and that is the load-bearing one: a harness must never
// run inside anything that retries (§8.2 rule 7), and everything downstream —
// whether its evidence can license another cell's attempt, whether a
// transaction may re-run it — reads this field rather than asking what it is.
func (h *Harness) Properties() cell.ExecutorProperties {
	return cell.ExecutorProperties{
		Loops:         true,
		ToolsAttached: h.SocketEnv != "",
		ConsumesLease: true,
		EditsInPlace:  true,
	}
}

// Fragment implements cell.Executor, measured once and cached.
//
// The measurement is of the harness binary, not of the model behind it. A
// harness that reaches a hosted model cannot prove what served a request, and a
// fragment that claimed to would be a guess with a version number on it.
func (h *Harness) Fragment(ctx context.Context) (cell.Fragment, error) {
	h.once.Do(func() {
		if len(h.VersionArgs) == 0 {
			h.fragErr = fmt.Errorf("%w: no version_args configured for %s", cell.ErrIndescribable, h.Path)
			return
		}
		out, err := h.probe(ctx)
		if err != nil {
			h.fragErr = fmt.Errorf("%w: %v", cell.ErrIndescribable, err)
			return
		}
		model := h.Model
		if model == "" {
			// No model configured is honest for a harness whose model is
			// chosen elsewhere — its own configuration, a hosted default. The
			// fragment says what was measured and stays quiet about the rest.
			h.fragment = cell.Fragment{Toolchains: map[string]string{"harness": out}}
			return
		}
		h.fragment = cell.Fragment{
			Toolchains: map[string]string{"harness": out},
			Model:      &cell.EnvModel{ID: model, Version: h.ModelVersion},
		}
	})
	return h.fragment, h.fragErr
}

func (h *Harness) probe(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, h.Path, h.VersionArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("probing %s: %v", h.Path, err)
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		out = strings.TrimSpace(stderr.String())
	}
	if out == "" {
		return "", fmt.Errorf("%s produced no version", h.Path)
	}
	// First line only: a version banner that also prints a build date or a
	// config path would otherwise make the environment hash change for reasons
	// that are not the environment changing.
	return strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), nil
}

// ErrNoCheckout is returned when a harness is asked to work with no directory.
//
// A refusal rather than a fallback to the cell's own working directory, which
// is the mistake worth preventing by name: a harness pointed at the cell's
// checkout instead of the task's would edit the wrong tree, and the edits would
// look exactly like a successful attempt.
var ErrNoCheckout = errors.New("executor: a harness edits a task checkout and was given no directory")

// Author implements Authoring by running the harness in the task checkout.
//
// What comes back in Response.Text is the harness's own account of what it did.
// It is recorded and never applied — the caller knows that from EditsInPlace,
// and applying it would overwrite the files the harness just wrote with its
// description of them.
func (h *Harness) Author(ctx context.Context, r Request) (Response, error) {
	if r.Dir == "" {
		return Response{}, ErrNoCheckout
	}

	args := append([]string(nil), h.Args...)
	var stdin string
	if h.PromptArg != "" {
		args = append(args, h.PromptArg, r.Intent)
	} else {
		stdin = r.Intent
	}

	cmd := exec.CommandContext(ctx, h.Path, args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), h.Env...)
	if h.SocketEnv != "" && r.Socket != "" {
		cmd.Env = append(cmd.Env, h.SocketEnv+"="+r.Socket)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	text := strings.TrimSpace(stdout.String())
	if err != nil {
		// A harness that exits nonzero may still have edited files, and those
		// edits are on disk whatever this returns. Saying so here is the
		// difference between a caller that looks and one that assumes nothing
		// happened — the loop asks varvig what changed rather than trusting
		// either of us.
		return Response{Text: text}, fmt.Errorf("harness %s: %v: %s",
			h.Path, err, strings.TrimSpace(stderr.String()))
	}
	return Response{Text: text}, nil
}
