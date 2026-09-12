// Package executor implements the cell.Executor seam (FACTORY.md §4): the
// executors a cell actually runs, and the two shapes of work they take.
//
// The common contract — cell.Executor, cell.ExecutorProperties and
// cell.ErrIndescribable — lives in the cell package, with the environment
// fragment it is obliged to produce. It is there rather than here so that
// `cell.Executor` and `effect.Executor` are visibly different things: one
// performs regenerable work for a cell, the other reaches a world that charges
// money. What stays here is the implementations, and the two interfaces that
// say which kind of work an executor takes.
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
// There is no model-runtime adapter as a cell concern any more. Inference
// belongs to whichever executor performs it, along with its auth and its model
// choice — one thing owning one concern.
//
// # Properties, not kinds
//
// An executor declares what it is like, never what it *is*. Nothing here
// switches on a kind, and nothing should: the wiring an executor gets is
// proportional to the properties it declares (§4.5), so a new shape of executor
// changes a table of behaviour rather than a list of cases. A guard fails the
// build on any type assertion or type switch over a type from this package,
// outside this package.
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

// Authoring is an executor that turns intent into a candidate change.
//
// Checking and authoring are separate interfaces rather than one Execute method
// over a union, because the work genuinely differs and a union would mean half
// the fields meaningless on every call, with nothing stopping a caller handing
// checking work to a model. Asking "does this executor author?" in the type
// system is not the same as asking what kind it is: a harness and a native loop
// both answer yes, and the caller still cannot tell them apart.
type Authoring interface {
	cell.Executor
	// Author runs one authoring request.
	Author(ctx context.Context, req Request) (Response, error)
}

// Checking is an executor that measures — a build, a test run, a lint.
type Checking interface {
	cell.Executor
	// Check runs one job. A job that exits nonzero is a StatusFail result and a
	// nil error: a failing test is a measurement, not a malfunction. Check
	// returns an error only when the measurement could not be taken at all.
	Check(ctx context.Context, job Job) (Result, error)
}
