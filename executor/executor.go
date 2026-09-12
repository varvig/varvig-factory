// Package executor is the seam everything that performs work sits behind
// (FACTORY.md §4): a native agent loop, an agent harness, a build and test
// runner, a 3D printer.
//
// # Two seams, not four
//
// This package is the result of folding the model-runtime adapter and the build
// sandbox into one. They were two adapters with the same shape — name yourself,
// describe your environment, do a unit of work — and keeping them apart made
// "how does a cell run a model" a different question from "how does a cell run a
// test", which meant adding a harness would have been a third answer to a
// question that should only have one.
//
// The rule the fold enforces: **a new executor must never require a new Factory
// concept.** A Claude Code harness is an executor whose properties say it loops
// and takes tools. A CNC machine is an executor whose properties say it is
// effectful. Neither needs a package, an interface, or a config section of its
// own, and that is the test of whether this seam is doing its job.
//
// There is no model-runtime adapter as a cell concern any more. Inference
// belongs to whichever executor performs it, along with its auth and its model
// choice — one thing owning one concern.
//
// # Properties, not kinds
//
// An executor declares what it is like, never what it *is*. Nothing here
// switches on a kind, and nothing should: the wiring an executor gets is
// proportional to the properties it declares (§4.5), so a new shape of executor
// changes a table of behaviour rather than a list of cases.
//
// # What is not built yet
//
// §4.5's wiring table has two rows and only one of them exists. Everything here
// runs **in the cell's process**: one call, no checkout, no subprocess, no MCP
// socket. The `tools_attached: true` row — a sandboxed subprocess with a sparse
// checkout and a per-task socket at /run/varvig/task-<id>.sock — has no
// implementation, so ToolsAttached is a property an executor may declare and
// nothing yet acts on.
//
// That is worth stating rather than leaving to be discovered, because the
// property being *declarable* is what makes the missing row an addition instead
// of a redesign: whoever builds it branches on ToolsAttached, and no existing
// executor changes.
package executor

import (
	"context"

	"github.com/varvig/varvig-factory/cell"
)

// Properties are what an executor declares about itself (§4.1).
//
// They are deliberately adjectives rather than a type name. "Loops and takes
// tools" is something a caller can act on — it decides checkout, socket and
// subprocess — where "is a Claude Code harness" is something a caller would
// have to already know about to act on.
type Properties struct {
	// Loops says the executor iterates on feedback rather than answering once.
	Loops bool
	// ToolsAttached says it receives a task MCP socket, which is the line
	// between thinking and doing (§4.2): authority attaches to the tool
	// channel, never to the model.
	ToolsAttached bool
	// Deterministic says its results are evidence-comparable and cacheable. A
	// test runner is; a model is not, and that difference is why a transaction
	// may re-run one and never the other (§4.4).
	Deterministic bool
	// ConsumesLease says running it spends budget. An agent does; a test run
	// mostly does not.
	ConsumesLease bool
	// Effectful says it has real-world side effects — the §6.7 class, here just
	// a property.
	Effectful bool
}

// Executor is what every executor is, whatever work it takes.
//
// It is the whole of the common contract, and it is small on purpose: a seam
// that demanded more would be a seam that had opinions about what work looks
// like, which is exactly what made two adapters out of one idea.
type Executor interface {
	// Name identifies the executor for logs and errors. Not part of the
	// environment: the fragment is.
	Name() string

	// Properties is what this executor declares about itself (§4.1).
	Properties() Properties

	// Fragment reports this executor's slice of the environment descriptor
	// (CELL.md §4.2, §6), measured from what it will actually run.
	//
	// It must be deterministic across invocations, and a measurement rather
	// than a configured claim. An executor that cannot describe itself
	// reproducibly returns ErrIndescribable — emitting a guessed environment
	// would make every downstream cross-cell comparison a comparison of
	// guesses.
	Fragment(ctx context.Context) (cell.Fragment, error)
}

// Authoring is an executor that turns intent into a candidate change.
//
// Checking and authoring are separate interfaces rather than one Execute method
// over a union, because the work genuinely differs and a union would mean half
// the fields meaningless on every call, with nothing stopping a caller handing
// checking work to a model. Asking "does this executor author?" in the type
// system is not the same as asking what kind it is: a harness and a native loop
// both answer yes, and the caller still cannot tell them apart.
type Authoring interface {
	Executor
	// Author runs one authoring request.
	Author(ctx context.Context, req Request) (Response, error)
}

// Checking is an executor that measures — a build, a test run, a lint.
type Checking interface {
	Executor
	// Check runs one job. A job that exits nonzero is a StatusFail result and a
	// nil error: a failing test is a measurement, not a malfunction. Check
	// returns an error only when the measurement could not be taken at all.
	Check(ctx context.Context, job Job) (Result, error)
}
