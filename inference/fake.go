package inference

import (
	"context"
	"fmt"

	"github.com/varvig/varvig-factory/cell"
)

// Fake is a deterministic Runtime for tests and for the runnable demo. It is in
// the non-test build for the same reason tracker.Mem is in varvig-connectors':
// the whole cell lifecycle has to be exercisable with no GPU, no server, and no
// spend, or the §9 test suite would only ever run on the machine that has one.
type Fake struct {
	// Reply is returned verbatim by Generate. If Replies is non-empty it takes
	// precedence, indexed by attempt number (1-based), so a test can make two
	// attempts differ — which is what makes selection between duplicates
	// testable at all.
	Reply   string
	Replies []string
	// Err, if set, is returned by Generate.
	Err error
	// Model and Version appear in the fragment.
	Model, Version string
	// RuntimeVersion is the measured runtime token in the fragment. Two Fakes
	// with different values are deliberately different environment classes.
	RuntimeVersion string
	// Params appear in the fragment.
	Params Params
	// Indescribable makes Fragment fail, for the test that a cell refuses an
	// adapter that cannot describe itself.
	Indescribable bool

	// Calls counts Generate invocations, so a test can assert that a halted
	// cell stopped spending rather than merely stopped reporting.
	Calls int
}

// Name implements Runtime.
func (f *Fake) Name() string { return "fake" }

// Fragment implements Runtime.
func (f *Fake) Fragment(context.Context) (cell.Fragment, error) {
	if f.Indescribable {
		return cell.Fragment{}, fmt.Errorf("%w: fake configured as indescribable", ErrIndescribable)
	}
	version := f.RuntimeVersion
	if version == "" {
		version = "fake-1"
	}
	model := f.Model
	if model == "" {
		model = "fake-model"
	}
	return cell.Fragment{
		Toolchains: map[string]string{"inference-runtime": version},
		Model:      &cell.EnvModel{ID: model, Version: f.Version, Params: f.Params.String()},
	}, nil
}

// Generate implements Runtime.
func (f *Fake) Generate(_ context.Context, r Request) (Response, error) {
	f.Calls++
	if f.Err != nil {
		return Response{}, f.Err
	}
	text := f.Reply
	if n := len(f.Replies); n > 0 {
		i := r.Attempt - 1
		if i < 0 {
			i = 0
		}
		if i >= n {
			i = n - 1
		}
		text = f.Replies[i]
	}
	return Response{Text: text, TokensIn: len(Prompt(r)) / 4, TokensOut: len(text) / 4}, nil
}
