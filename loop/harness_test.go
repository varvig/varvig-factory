package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/executor"
	"github.com/varvig/varvig-factory/varvigcli"
)

const harnessTicket = "b17e5500000000000000000000000000000000000000000000000000000000ab"

// inPlace is an executor that edits the checkout and reports, which is what a
// harness does. Declared in a test file for the same reason the harness vector
// in executor_test.go is: if exercising a new shape of executor required
// touching the loop, the claim policy or the cell contract, the seam would not
// be doing its job.
type inPlace struct {
	// writes maps a path inside the checkout to its content.
	writes map[string]string
	// report is what it says it did, which must never be applied as content.
	report string
	// dir and socket record what the loop handed it.
	dir, socket string
	tools       bool
}

func (e *inPlace) Name() string { return "in-place" }

func (e *inPlace) Properties() cell.ExecutorProperties {
	return cell.ExecutorProperties{Loops: true, EditsInPlace: true, ConsumesLease: true, ToolsAttached: e.tools}
}

func (e *inPlace) Fragment(context.Context) (cell.Fragment, error) {
	return cell.Fragment{Toolchains: map[string]string{"harness": "1.0"}}, nil
}

func (e *inPlace) Author(_ context.Context, r executor.Request) (executor.Response, error) {
	e.dir, e.socket = r.Dir, r.Socket
	for path, body := range e.writes {
		full := filepath.Join(r.Dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return executor.Response{}, err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return executor.Response{}, err
		}
	}
	return executor.Response{Text: e.report, TokensIn: 10, TokensOut: 20}, nil
}

func harnessCell(t *testing.T, e executor.Authoring) (*Cell, *varvigcli.Fake) {
	t.Helper()
	v := varvigcli.NewFake("mini-a")
	fr, pr := varvigcli.Collapsed(v)
	v.AddTicket(harnessTicket, "Add a thing.",
		varvigcli.Scope{Reads: []string{"src"}, Writes: []string{"src"}}, "approved")
	ledger, err := budget.NewLedger(
		budget.Budget{InferenceDaily: 100000, VerifyConcurrent: 1, StorageGB: 1, AttemptsDefault: 1, PerCallCost: 1},
		filepath.Join(t.TempDir(), "ledger.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &Cell{
		Capabilities: cell.Capabilities{CellID: "mini-a"},
		Factory:      fr,
		Project:      pr,
		Authoring:    e,
		Ledger:       ledger,
		WorkDir:      t.TempDir(),
		Log:          func(string) {},
	}, v
}

func harnessTicketOf(t *testing.T, v *varvigcli.Fake) claim.Ticket {
	t.Helper()
	spec, err := v.Spec(harnessTicket)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := v.Scope(harnessTicket)
	if err != nil {
		t.Fatal(err)
	}
	return claim.Ticket{ID: harnessTicket, Object: harnessTicket, Spec: spec, Scope: scope, Status: "approved"}
}

// TestAnInPlaceExecutorsReportIsNeverAppliedAsContent is the bug this property
// exists to prevent, and it is not hypothetical: a harness that edits
// src/a.go and then *describes* the edit in its summary — as any harness
// reporting its work naturally would — writes one thing and says another in the
// same shape. A caller that parsed the summary would overwrite the file with the
// prose description of it, and the tests would then measure something nobody
// wrote.
func TestAnInPlaceExecutorsReportIsNeverAppliedAsContent(t *testing.T) {
	e := &inPlace{
		writes: map[string]string{"src/a.go": "package src\n\nfunc Real() {}\n"},
		// The report quotes the change back in exactly the shape ApplyOutput
		// parses. This is what a helpful harness writes.
		report: "I added Real() to src/a.go:\n\n--- src/a.go\npackage src\n\n// a summary, not the file\n",
	}
	c, v := harnessCell(t, e)

	res, err := c.attempt(context.Background(), harnessTicketOf(t, v), 1, false)
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(e.dir, "src/a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "a summary, not the file") {
		t.Fatal("the harness's report was applied over the file it had just written; the attempt now holds a description of the change instead of the change")
	}
	if !strings.Contains(string(body), "func Real()") {
		t.Fatalf("the harness's actual edit did not survive: %q", body)
	}
	if res.Change == "" {
		t.Fatal("an attempt that edited a file produced no change")
	}
}

// TestTheLoopAsksVarvigWhatAnInPlaceExecutorChanged covers the other half:
// with nothing to parse, "did anything happen" has to be answered by looking.
//
// It matters because `varvig commit` on a clean tree succeeds and records an
// empty change, so a loop that committed blind would fill the speculation pool
// with empty candidates — each one a real candidate another cell might score.
func TestTheLoopAsksVarvigWhatAnInPlaceExecutorChanged(t *testing.T) {
	idle := &inPlace{report: "I looked at the ticket and there is nothing to do."}
	c, v := harnessCell(t, idle)

	res, err := c.attempt(context.Background(), harnessTicketOf(t, v), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Called("Changed") {
		t.Fatalf("the loop never asked what changed; calls: %v", v.Calls)
	}
	if res.Change != "" {
		t.Fatal("a harness that edited nothing produced a change; the pool now holds an empty candidate")
	}
	if v.Called("Commit") {
		t.Fatalf("a clean checkout was committed anyway; calls: %v", v.Calls)
	}
}

// TestAnInPlaceExecutorIsHeldToTheDeclaredWriteSet is the check ApplyOutput
// makes for text executors, applied where it was missing.
//
// varvig serializes on the scope a ticket declares. An attempt that touched
// paths outside it is a change claiming one scope and holding another, so the
// overlap varvig computed for this ticket was computed against the wrong set —
// and nothing downstream would notice.
func TestAnInPlaceExecutorIsHeldToTheDeclaredWriteSet(t *testing.T) {
	stray := &inPlace{writes: map[string]string{
		"src/fine.go":   "package src\n",
		"infra/prod.tf": "// not in the write set\n",
	}}
	c, v := harnessCell(t, stray)

	_, err := c.attempt(context.Background(), harnessTicketOf(t, v), 1, false)
	if err == nil {
		t.Fatal("a harness wrote outside the ticket's declared write set and the attempt was accepted")
	}
	if !strings.Contains(err.Error(), "infra/prod.tf") {
		t.Fatalf("the refusal does not name the offending path: %v", err)
	}
	// Refused rather than trimmed to the part that was in scope: committing
	// half of whatever the harness was doing is its own failure.
	if v.Called("Commit") {
		t.Fatalf("the in-scope subset was committed anyway; calls: %v", v.Calls)
	}
}

// TestAToolTakingExecutorIsRefusedWithoutASocket: a harness with no tool
// channel is not a harness having a quiet day. It will do its work by guessing
// at a repository it cannot read, and bill for it.
func TestAToolTakingExecutorIsRefusedWithoutASocket(t *testing.T) {
	e := &inPlace{tools: true, writes: map[string]string{"src/a.go": "package src\n"}}
	c, v := harnessCell(t, e)

	_, err := c.attempt(context.Background(), harnessTicketOf(t, v), 1, false)
	if err == nil {
		t.Fatal("an executor that takes tools ran with no socket")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("the refusal does not say how to get a socket: %v", err)
	}

	// With a daemon serving one, the same cell runs and the socket reaches the
	// executor — authority on the channel, which is the point of passing it.
	v.TaskSocket = "/run/varvig/task-abc.sock"
	if _, err := c.attempt(context.Background(), harnessTicketOf(t, v), 1, false); err != nil {
		t.Fatal(err)
	}
	if e.socket != "/run/varvig/task-abc.sock" {
		t.Fatalf("the task socket did not reach the executor: %q", e.socket)
	}
}

// TestAnExecutorThatTakesNoToolsIsHandedNoSocket is the other side of §4.2.
//
// Authority attaches to the tool channel, so handing one to an executor that
// never declared it wants tools gives it a credential nobody decided to grant.
// The socket is scoped and propose-only, so the blast radius is bounded — but
// "bounded" is not "intended", and the wiring an executor gets is supposed to be
// proportional to what it declares.
func TestAnExecutorThatTakesNoToolsIsHandedNoSocket(t *testing.T) {
	quiet := &inPlace{writes: map[string]string{"src/a.go": "package src\n"}}
	c, v := harnessCell(t, quiet)
	v.TaskSocket = "/run/varvig/task-abc.sock"

	if _, err := c.attempt(context.Background(), harnessTicketOf(t, v), 1, false); err != nil {
		t.Fatal(err)
	}
	if quiet.socket != "" {
		t.Fatalf("an executor declaring no tools was handed the task socket %q", quiet.socket)
	}
	// It still gets the checkout, which it did declare.
	if quiet.dir == "" {
		t.Fatal("an in-place executor was given no directory")
	}
}

// TestATextExecutorIsUnaffected: the in-place path is an addition, and the
// executor that answers with content still has its output applied.
func TestATextExecutorIsUnaffected(t *testing.T) {
	c, v := harnessCell(t, &executor.FakeAuthor{
		Reply: "--- src/a.go\npackage src\n\nfunc FromText() {}\n",
	})
	res, err := c.attempt(context.Background(), harnessTicketOf(t, v), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Change == "" {
		t.Fatal("a text executor's output was not applied")
	}
	if v.Called("Changed") {
		t.Fatalf("the loop asked varvig what changed for an executor that answers with content; calls: %v", v.Calls)
	}
}
