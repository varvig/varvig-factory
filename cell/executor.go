package cell

import (
	"context"
	"errors"
)

// Executor is what every executor is, whatever work it takes (FACTORY.md §4).
//
// It lives in the cell contract rather than in the package that implements it,
// and the reason is a name collision that was costing a reader real confusion.
// There are two seams in this module with a legitimate claim to the word:
//
//	cell.Executor      performs work FOR a cell — runs a model, runs a test
//	effect.Executor    causes something IRREVERSIBLE outside it (§8.2)
//
// They are not variations on one idea, and the line between them is
// reversibility rather than cost. Work done by a cell.Executor is regenerable:
// if it comes out wrong, run it again. Work done by an effect.Executor cannot
// be taken back — which is why it needs a reservation, a higher principal's
// authorization, and every refusal in `effect`, whether or not it costs
// anything. Effectful and expensive are orthogonal (§8.0): turning on a light
// is free and still irreversible, so it is bounded by the envelope's quantity
// and rate ceilings instead of by a lease.
//
// Two types named Executor in one module is a thing a reader trips over exactly
// once, and they should not have to. Note also that an effect executor may run
// inside the cell or in a separate connector peer; that is a deployment choice
// within the effect seam, not a third kind of executor.
//
// Putting the interface here also puts it where its obligations already live:
// what it returns is a Fragment (§4.2), what it must not do is guess one, and
// both of those are this document's rules rather than any implementation's.
// A third-party executor needs this package and nothing else to satisfy the
// contract.
//
// # Small on purpose
//
// This is the whole of the common contract. A seam that demanded more would be
// a seam with opinions about what work looks like, which is exactly what made
// two adapters out of one idea — the model-runtime adapter and the build
// sandbox had the same shape, and keeping them apart made "how does a cell run
// a model" a different question from "how does a cell run a test".
//
// The rule that folding them enforces: **a new executor must never require a
// new Factory concept.** A harness is an executor whose properties say it loops
// and takes tools. A CNC machine is an executor whose properties say it is
// effectful. Neither needs a package, an interface, or a config section of its
// own, and that is the test of whether this seam is doing its job.
type Executor interface {
	// Name identifies the executor for logs and errors. Not part of the
	// environment: the fragment is.
	Name() string

	// Properties is what this executor declares about itself (§4.1).
	Properties() ExecutorProperties

	// Fragment reports this executor's slice of the environment descriptor
	// (§4.2, §6), measured from what it will actually run.
	//
	// It must be deterministic across invocations, and a measurement rather
	// than a configured claim. An executor that cannot describe itself
	// reproducibly returns ErrIndescribable — emitting a guessed environment
	// would make every downstream cross-cell comparison a comparison of
	// guesses.
	Fragment(ctx context.Context) (Fragment, error)
}

// ExecutorProperties are what an executor declares about itself (§4.1).
//
// Named for what they describe rather than shortened to Properties, because in
// this package a bare Properties would sit beside Capabilities and read as
// facts about the cell. These are facts about one executor the cell holds.
//
// They are deliberately adjectives rather than a type name. "Loops and takes
// tools" is something a caller can act on — it decides checkout, socket and
// subprocess — where "is a Claude Code harness" is something a caller would
// have to already know about to act on.
type ExecutorProperties struct {
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
	// EditsInPlace says the executor writes into the checkout itself, so its
	// response is a **report of work already done** rather than content to
	// apply.
	//
	// The two are not interchangeable and getting it wrong is not a no-op. A
	// caller that parses a harness's summary as file content will happily
	// overwrite the files the harness just wrote with whatever its prose
	// quoted back — the summary of a change, mistaken for the change.
	//
	// Independent of Loops, deliberately. A multi-turn conversation that ends
	// by emitting a patch loops without editing anything; a codemod script
	// edits in place without looping.
	EditsInPlace bool
	// Effectful says it has real-world side effects — the §8.2 class, here just
	// a property.
	//
	// An executor declaring this is not thereby authorized to act. Authority to
	// spend is a lease, and the refusals that stand between a declaration and
	// an irreversible action live in `effect`.
	Effectful bool
}

// ErrIndescribable is returned by an executor that cannot report a reproducible
// environment fragment.
//
// One error for every executor, because it is one rule: an executor that cannot
// describe itself cannot participate in cross-cell selection at all (§4), and
// that is as true of a test runner as of a model. Having had one of these per
// adapter was a small symptom of the split the fold removed.
//
// It lives with the interface that states the rule, so an executor written
// outside this module has one package to import rather than two — and so that
// the contract does not describe a sentinel defined downstream of itself.
var ErrIndescribable = errors.New("cell: executor cannot describe its environment reproducibly")
