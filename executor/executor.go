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
// # Both wiring rows now exist
//
// §4.5's table has two. HTTP, Command and None are the first: one call, in the
// cell's process, no checkout and no socket. Harness is the second: a
// subprocess with the task checkout as its working directory and the task's MCP
// socket on its environment.
//
// Adding the second changed no existing executor and added no case to any
// switch, which was the claim the fold rested on. What it did add is two fields
// on Request (Dir, Socket) and one property (EditsInPlace) — a wider table, not
// a new concept.
//
// One thing the second row is **not**, despite §4.5 calling it sandboxed: a
// sandbox. The subprocess runs with this cell's user and filesystem access, and
// the checkout confines it only by convention. The confinement that is real is
// the socket, which is scoped and propose-only.
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
