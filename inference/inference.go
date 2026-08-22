// Package inference is the model-runtime seam (FACTORY.md §4). Everything
// vendor- or hardware-shaped about running a model lives behind it, so neither
// varvig nor Factory's core loop learns about CUDA, quantization, or a
// particular vendor's request envelope.
//
// There are two implementations and they cover all four rows of the spec's
// table. ollama, vLLM and llama.cpp's server are HTTP endpoints speaking the
// widely-implemented chat-completions shape, as are hosted APIs; llama.cpp's
// CLI and any local wrapper script are subprocesses. Which one a cell uses, and
// therefore whether it is Micro or Mini, is configuration — not a code path
// (§1.2). If a tier ever needs a branch in this package, the abstraction has
// failed.
package inference

import (
	"context"
	"errors"
	"fmt"

	"github.com/varvig/varvig-factory/cell"
)

// Request is one authoring task handed to a model.
//
// It carries intent and context and nothing about ordering: a Request has no
// read set, no write set, and no priority, because deciding those would make
// this package a second scheduler (CELL.md §10.1). varvig decides what may run
// against what; this seam only produces candidate text.
type Request struct {
	// Task is the varvig ticket id, for logging and cost attribution.
	Task string
	// Intent is the ticket's spec, verbatim as varvig printed it.
	Intent string
	// Context is supporting material — file contents from the task's read set,
	// already fetched by the caller through varvig's scoped gate.
	Context []ContextFile
	// Attempt distinguishes repeated tries at the same task, so a runtime that
	// wants to vary sampling across attempts can, without the loop telling it
	// how.
	Attempt int
	// MaxTokens bounds the response. Zero means the runtime's own default.
	MaxTokens int
}

// ContextFile is one piece of supporting material.
type ContextFile struct {
	Path    string
	Content string
}

// Response is what a runtime produced.
type Response struct {
	// Text is the model's output, verbatim. Interpreting it — as a patch, as a
	// set of file writes — is the caller's job, because that interpretation is
	// repository-shaped and this seam is model-shaped.
	Text string
	// TokensIn and TokensOut are what the runtime reported, zero if it reported
	// nothing. They feed the budget ledger (FACTORY.md §7).
	TokensIn, TokensOut int
	// Cost is the spend this call incurred in the ledger's unit, if the runtime
	// can attribute it. Zero means "not attributable here" and the caller
	// prices it from tokens instead.
	Cost float64
}

// Runtime is the model-runtime seam.
type Runtime interface {
	// Name identifies the adapter for logs and errors. Not part of the
	// environment: the fragment is.
	Name() string

	// Fragment reports this adapter's slice of the environment descriptor
	// (CELL.md §4.2, §6), measured from the runtime it will actually call.
	//
	// It must be deterministic across invocations, and it must be a
	// measurement rather than a configured claim. An adapter that cannot
	// describe itself reproducibly returns ErrIndescribable and the cell
	// refuses to use it — emitting a guessed environment would make every
	// downstream cross-cell comparison a comparison of guesses.
	Fragment(ctx context.Context) (cell.Fragment, error)

	// Generate runs one authoring request.
	Generate(ctx context.Context, req Request) (Response, error)
}

// ErrIndescribable is returned by an adapter that cannot report a reproducible
// environment fragment. It is a configuration error, surfaced at startup rather
// than at the first attempt, because a cell that cannot describe its
// environment cannot participate in cross-cell selection at all (§4).
var ErrIndescribable = errors.New("inference: adapter cannot describe its environment reproducibly")

// Params are the sampling parameters that affect output. They are recorded in
// the environment descriptor's model.params field, canonically, so that two
// cells sampling differently are visibly not the same ground.
type Params struct {
	Temperature float64
	TopP        float64
	Seed        int64
}

// String renders params canonically: sorted, fixed formatting, empty when
// nothing is set. It is the value that lands in the environment hash, so its
// spelling is stable rather than convenient.
func (p Params) String() string {
	var parts []string
	if p.Temperature != 0 {
		parts = append(parts, fmt.Sprintf("temp=%g", p.Temperature))
	}
	if p.TopP != 0 {
		parts = append(parts, fmt.Sprintf("top_p=%g", p.TopP))
	}
	if p.Seed != 0 {
		parts = append(parts, fmt.Sprintf("seed=%d", p.Seed))
	}
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

// None is the runtime for a cell with no model at all — a Micro cell whose
// roles are verify and build (FACTORY.md §3.1). It describes itself honestly
// (an empty fragment: it contributes no model, because there is none) and
// refuses to generate.
//
// This exists so that "no model" is a configuration rather than a nil check
// scattered through the loop. A cell without RoleAttempt never calls Generate;
// if a misconfiguration makes it, the refusal is explicit and names the reason.
type None struct{}

// Name implements Runtime.
func (None) Name() string { return "none" }

// Fragment implements Runtime. A cell with no model contributes no model field:
// build and test evidence must not carry one, or deterministic evidence would
// look sampled (CELL.md §4.2).
func (None) Fragment(context.Context) (cell.Fragment, error) { return cell.Fragment{}, nil }

// Generate implements Runtime by refusing.
func (None) Generate(context.Context, Request) (Response, error) {
	return Response{}, errors.New("inference: this cell has no model runtime; it can verify and build but not attempt")
}
