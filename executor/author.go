// Authoring executors: the ones that turn intent into a candidate change.
//
// Everything vendor- or hardware-shaped about running a model lives behind this
// seam, so neither varvig nor the cell loop learns about CUDA, quantization, or
// a particular vendor's request envelope.
//
// Two implementations cover the field. ollama, vLLM and llama.cpp's server are
// HTTP endpoints speaking the widely-implemented chat-completions shape, as are
// hosted APIs; llama.cpp's CLI and any local wrapper script are subprocesses.
// Which one a cell uses is configuration, not a code path — and note what it is
// *not* evidence of: a hosted endpoint and a local server differ in latency and
// billing, not in anything the cell branches on, which is why cell class says
// nothing about inference (§3).
//
// A harness executor — Claude Code or equivalent — is another implementation of
// this same interface and needs nothing else. That is the fold working: before
// it, a harness would have been a third adapter beside a model runtime and a
// build sandbox, three answers to one question.

package executor

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
	Cost cell.Money
}

// ErrIndescribable is returned by an executor that cannot report a reproducible
// environment fragment.
//
// One error for every executor, because it is one rule: an executor that cannot
// describe itself cannot participate in cross-cell selection at all (§4), and
// that is as true of a test runner as of a model. Having had two of these, one
// per adapter, was a small symptom of the same split this fold removes.
var ErrIndescribable = errors.New("executor: cannot describe its environment reproducibly")

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
// scattered through the loop. A cell without RoleAttempt never calls Author;
// if a misconfiguration makes it, the refusal is explicit and names the reason.
type None struct{}

// Name implements Authoring.
func (None) Name() string { return "none" }

// Properties implements Executor. A cell with no model authors nothing, so
// every property is false — including ConsumesLease, which is the honest answer
// for an executor that never runs.
func (None) Properties() Properties { return Properties{} }

// Fragment implements Authoring. A cell with no model contributes no model field:
// build and test evidence must not carry one, or deterministic evidence would
// look sampled (CELL.md §4.2).
func (None) Fragment(context.Context) (cell.Fragment, error) { return cell.Fragment{}, nil }

// Author implements Authoring by refusing.
func (None) Author(context.Context, Request) (Response, error) {
	return Response{}, errors.New("inference: this cell has no model runtime; it can verify and build but not attempt")
}
