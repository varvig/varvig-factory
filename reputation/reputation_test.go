package reputation

import (
	"encoding/json"
	"testing"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// seed writes one attempt by cellID at task, producing change.
func seed(t *testing.T, p varvigcli.ProjectRepo, cellID, task, change string, n int) {
	t.Helper()
	att := cell.Attempt{
		CellID: cellID, Task: task, Change: change, N: n, CreatedAt: 1,
	}
	body, err := json.Marshal(att)
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.PutBlob(body)
	if err != nil {
		t.Fatal(err)
	}
	name, err := cell.AttemptRef(cellID, task, n)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateRef(name, id, ""); err != nil {
		t.Fatal(err)
	}
}

// promote records that a task was promoted. The observation is a note on the
// ticket *object*, which is how agreement finds them, so the task has to be a
// real ticket rather than a bare string.
func promote(t *testing.T, v *varvigcli.Fake, p varvigcli.ProjectRepo, task, top, promoted string) {
	t.Helper()
	v.AddTicket(task, "work", varvigcli.Scope{Reads: []string{"src"}}, "approved")
	obj := v.RefSnapshot()["refs/varvig/tickets/"+task]
	if obj == "" {
		t.Fatalf("ticket %s has no object", task)
	}
	if err := agreement.Record(p, obj, agreement.Observation{
		Scope: "src/", Task: task, TopAttempt: top, PromotedAttempt: promoted,
		Agreed: top == promoted, ObservedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStandingComesFromWhatWasPromoted, not from what a cell says about itself.
func TestStandingComesFromWhatWasPromoted(t *testing.T) {
	v := varvigcli.NewFake("project")
	_, p := varvigcli.Collapsed(v)

	// Two cells competed on two tasks. mini-a won both.
	seed(t, p, "mini-a", "task-1", "change-a1", 1)
	seed(t, p, "micro-b", "task-1", "change-b1", 1)
	promote(t, v, p, "task-1", "change-a1", "change-a1")

	seed(t, p, "mini-a", "task-2", "change-a2", 1)
	seed(t, p, "micro-b", "task-2", "change-b2", 1)
	promote(t, v, p, "task-2", "change-b2", "change-a2")

	got, err := Derive(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("derived %d standings, want 2: %+v", len(got), got)
	}
	if got[0].CellID != "mini-a" || got[0].Promoted != 2 || got[0].Attempts != 2 {
		t.Errorf("best standing = %+v, want mini-a with 2 of 2", got[0])
	}
	if got[1].CellID != "micro-b" || got[1].Promoted != 0 || got[1].Attempts != 2 {
		t.Errorf("second standing = %+v, want micro-b with 0 of 2", got[1])
	}
	if got[0].Rate() != 1.0 || got[1].Rate() != 0.0 {
		t.Errorf("rates = %v and %v, want 1 and 0", got[0].Rate(), got[1].Rate())
	}
}

// TestUnfinishedWorkIsNotALoss. An attempt at a task nobody has promoted yet is
// unfinished, not failed, and counting it would rank a cell down for working on
// what is still open.
func TestUnfinishedWorkIsNotALoss(t *testing.T) {
	v := varvigcli.NewFake("project")
	_, p := varvigcli.Collapsed(v)
	seed(t, p, "mini-a", "task-1", "change-a1", 1)
	promote(t, v, p, "task-1", "change-a1", "change-a1")
	// Still open: nobody has promoted task-2.
	seed(t, p, "mini-a", "task-2", "change-a2", 1)

	got, err := Of(p, "mini-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 1 || got.Promoted != 1 {
		t.Errorf("standing = %+v, want 1 of 1 — the open task must not count", got)
	}
}

// TestNoRecordIsNotAZeroRate. "No record" and "a record of losing" are opposite
// answers, and a new cell reading as the worst possible cell is how a metric
// becomes a barrier to entry.
func TestNoRecordIsNotAZeroRate(t *testing.T) {
	v := varvigcli.NewFake("project")
	_, p := varvigcli.Collapsed(v)
	got, err := Of(p, "newcomer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Rate() != -1 {
		t.Errorf("a cell with no record has rate %v, want -1", got.Rate())
	}

	seed(t, p, "mini-a", "task-1", "change-a1", 1)
	promote(t, v, p, "task-1", "change-a1", "change-a1")
	still, err := Of(p, "newcomer")
	if err != nil {
		t.Fatal(err)
	}
	if still.Rate() != -1 {
		t.Errorf("a cell that entered no attempts has rate %v, want -1", still.Rate())
	}
}

// TestAPeerPromotingAnotherCellsAttemptStillCounts: in a flat factory the cell
// that promotes is usually not the cell that produced the change, so matching
// has to be on the change rather than on who moved the ref.
func TestAPeerPromotingAnotherCellsAttemptStillCounts(t *testing.T) {
	v := varvigcli.NewFake("project")
	_, p := varvigcli.Collapsed(v)
	seed(t, p, "mini-a", "task-1", "change-a1", 1)
	seed(t, p, "micro-b", "task-1", "change-b1", 1)
	// micro-b verified and promoted mini-a's attempt.
	promote(t, v, p, "task-1", "change-a1", "change-a1")

	got, err := Of(p, "mini-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Promoted != 1 {
		t.Errorf("mini-a standing = %+v; the producer of the promoted change gets the credit", got)
	}
}

// TestNothingPromotedYetIsSilenceNotZeroes.
func TestNothingPromotedYetIsSilenceNotZeroes(t *testing.T) {
	v := varvigcli.NewFake("project")
	_, p := varvigcli.Collapsed(v)
	seed(t, p, "mini-a", "task-1", "change-a1", 1)
	got, err := Derive(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("derived %+v with nothing promoted; a factory with no promotions has no records, not empty ones", got)
	}
}
