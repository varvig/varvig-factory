package inference

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/varvig/varvig-factory/cell"
)

// CommandRuntime drives a model as a subprocess: llama.cpp's CLI, a local
// wrapper script, anything that reads a prompt on stdin and writes a completion
// on stdout. It is the fourth row of the §4 table, and the one that makes the
// seam obviously real — a runtime with no HTTP surface at all still plugs in.
type CommandRuntime struct {
	// Path is the executable.
	Path string
	// Args are passed to every invocation. The prompt goes on stdin rather than
	// in an argument: prompts are large and argv is not.
	Args []string
	// VersionArgs produce a version string on stdout or stderr, e.g.
	// {"--version"}. REQUIRED: without it the adapter cannot describe itself.
	VersionArgs []string
	// Model and ModelVersion identify the weights, recorded in the environment.
	Model, ModelVersion string
	// Params are recorded in the environment. Passing them to the process is
	// the operator's business, via Args — this adapter does not guess a
	// binary's flag spelling, because guessing wrong would mean the recorded
	// params and the actual sampling disagree, which is worse than not
	// configuring them.
	Params Params
	// Env are extra environment variables for the process.
	Env []string

	once     sync.Once
	fragment cell.Fragment
	fragErr  error
}

// Name implements Runtime.
func (c *CommandRuntime) Name() string { return "command" }

// Fragment implements Runtime by running VersionArgs once and caching the
// output. Measured from the binary that will actually run, per CELL.md §6.
func (c *CommandRuntime) Fragment(ctx context.Context) (cell.Fragment, error) {
	c.once.Do(func() { c.fragment, c.fragErr = c.probe(ctx) })
	return c.fragment, c.fragErr
}

func (c *CommandRuntime) probe(ctx context.Context) (cell.Fragment, error) {
	if c.Path == "" {
		return cell.Fragment{}, fmt.Errorf("inference: command runtime has no path configured")
	}
	if c.Model == "" {
		return cell.Fragment{}, fmt.Errorf("inference: command runtime has no model configured")
	}
	if len(c.VersionArgs) == 0 {
		return cell.Fragment{}, fmt.Errorf("%w: no version_args configured for %s", ErrIndescribable, c.Path)
	}
	out, err := probeVersion(ctx, c.Path, c.VersionArgs, c.Env)
	if err != nil {
		return cell.Fragment{}, fmt.Errorf("%w: %v", ErrIndescribable, err)
	}
	return cell.Fragment{
		Toolchains: map[string]string{"inference-runtime": out},
		Model: &cell.EnvModel{
			ID:      c.Model,
			Version: c.ModelVersion,
			Params:  c.Params.String(),
		},
	}, nil
}

// Generate implements Runtime.
func (c *CommandRuntime) Generate(ctx context.Context, r Request) (Response, error) {
	if c.Path == "" {
		return Response{}, fmt.Errorf("inference: command runtime has no path configured")
	}
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Stdin = strings.NewReader(Prompt(r))
	cmd.Env = append(cmd.Environ(), c.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Response{}, fmt.Errorf("inference: %s: %w: %s", c.Path, err, snippet(stderr.Bytes()))
	}
	// No token counts: a CLI usually reports none, and inventing an estimate
	// would put a fabricated number into the budget ledger. The ledger prices
	// an unattributed call at its configured per-call rate instead
	// (FACTORY.md §7).
	return Response{Text: stdout.String()}, nil
}

// probeVersion runs a version command and returns its first non-empty output
// line, trimmed. Only the first line is used: many tools print a version
// followed by build details that include a path or a timestamp, and those would
// make the environment hash vary between machines that are in fact the same
// class.
func probeVersion(ctx context.Context, path string, args, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(cmd.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("probing %s %s: %w: %s", path, strings.Join(args, " "), err, snippet(stderr.Bytes()))
	}
	out := firstLine(stdout.String())
	if out == "" {
		// Some tools print --version to stderr. Accept it rather than calling
		// the adapter indescribable over a stream choice.
		out = firstLine(stderr.String())
	}
	if out == "" {
		return "", fmt.Errorf("probing %s %s: no output", path, strings.Join(args, " "))
	}
	return out, nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
