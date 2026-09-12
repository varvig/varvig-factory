package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/executor"
)

// harness is a executor shaped like the thing the fold exists to make free: an
// agent harness that loops, takes a tool socket, and is not deterministic.
//
// It is declared here, in a test file, deliberately. If adding an executor of a
// genuinely new shape required touching the executor package, the loop, the
// claim policy or the cell contract, it could not be — and that requirement is
// the whole of FACTORY.md §4.1's "a new executor must never require a new
// Factory concept".
type harness struct{ calls int }

func (h *harness) Name() string { return "harness" }

func (h *harness) Properties() cell.ExecutorProperties {
	return cell.ExecutorProperties{Loops: true, ToolsAttached: true, ConsumesLease: true}
}

func (h *harness) Fragment(context.Context) (cell.Fragment, error) {
	return cell.Fragment{
		Model:      &cell.EnvModel{ID: "harness-model", Version: "1.0"},
		Toolchains: map[string]string{"harness": "1.0"},
	}, nil
}

func (h *harness) Author(context.Context, executor.Request) (executor.Response, error) {
	h.calls++
	return executor.Response{
		Text:      "--- src/a.go\npackage src\n\nfunc FromHarness() {}\n",
		TokensIn:  10,
		TokensOut: 20,
	}, nil
}

func TestANewExecutorNeedsNoNewFactoryConcept(t *testing.T) {
	// The claim under test is not that this harness works — it is trivial and
	// of course it works. It is that making it work required changing nothing:
	// no new interface, no new config kind, no case added anywhere. The proof
	// is that the only thing this test does differently from every other cell
	// test is assign a different value to the same field.
	c, v, _ := effectCell(t, 100000)
	h := &harness{}
	c.Authoring = h
	c.Capabilities.Roles = append(c.Capabilities.Roles, cell.RoleAttempt)
	c.Capabilities.Inference = cell.Inference{
		Tier:   cell.TierLarge,
		Models: []cell.Model{{ID: "harness-model"}},
	}
	c.Capabilities.Build = []string{"go"}
	_ = v

	// It is reachable, which is all the loop asks of an authoring executor.
	ok, why := c.executorReachable(context.Background())
	if !ok {
		t.Fatalf("a harness executor was not reachable: %s", why)
	}

	// And the properties it declares are readable without anyone asking what it
	// is. A caller wiring a checkout and an MCP socket branches on this, never
	// on the type — which is what the guard in guard/ holds.
	props := c.Authoring.Properties()
	if !props.ToolsAttached || !props.Loops {
		t.Fatalf("the harness's declared properties did not survive the seam: %+v", props)
	}
	if props.Deterministic {
		t.Fatal("an agent harness declared itself deterministic; a transaction would then be free to re-run it")
	}
}

func TestBothRolesShareOneSeam(t *testing.T) {
	// The fold in one assertion: an authoring executor and a checking executor
	// are the same kind of thing, and a caller that only needs the common
	// contract can hold either in one variable.
	//
	// Before the fold this did not compile, because there was no type both an
	// inference.Runtime and a sandbox.Sandbox satisfied — which is exactly why
	// a harness would have needed a third adapter.
	//
	// The contract is cell.Executor rather than executor.Executor so that it is
	// visibly not effect.Executor: this one performs regenerable work for a
	// cell, that one reaches a world that charges money.
	var seams []cell.Executor
	seams = append(seams, &executor.FakeAuthor{Reply: "x"})
	seams = append(seams, &executor.FakeChecker{})
	seams = append(seams, &harness{})

	names := make([]string, 0, len(seams))
	for _, e := range seams {
		if _, err := e.Fragment(context.Background()); err != nil {
			t.Fatalf("%s could not describe itself: %v", e.Name(), err)
		}
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "fake,fake,harness" {
		t.Fatalf("names = %q", got)
	}

	// Exactly one of them is deterministic, and it is the one that measures.
	// That asymmetry is the seam's most load-bearing property (§4.4): evidence
	// may be re-run, authoring may not.
	deterministic := 0
	for _, e := range seams {
		if e.Properties().Deterministic {
			deterministic++
		}
	}
	if deterministic != 1 {
		t.Fatalf("%d of 3 executors declared themselves deterministic, want exactly 1 (the checker)", deterministic)
	}
}
