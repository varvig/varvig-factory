package promote

import (
	"testing"

	"github.com/varvig/varvig-factory/varvigcli"
)

// observeFixture builds a promoter over a fake project with one scored candidate
// for a ticket, and returns the promoter, the fake, and the shared ids.
func observeFixture(t *testing.T) (*Promoter, *varvigcli.Fake, string, string) {
	t.Helper()
	const ticket, cand = "ticket-1", "cand000000000000000000000000000000000000000000000000000000cand"
	f := varvigcli.NewFake("p")
	if err := f.SpecAdd(ticket, cand); err != nil {
		t.Fatal(err)
	}
	if err := f.SpecScore(ticket, cand, 1.0); err != nil {
		t.Fatal(err)
	}
	p := &Promoter{Project: varvigcli.ProjectRepo{Varvig: f}}
	return p, f, ticket, cand
}

func observe(t *testing.T, p *Promoter, ticket, ref string) (agreed bool, top, promoted string) {
	t.Helper()
	obs, err := p.ObservePromotion(Request{
		Ticket: ticket, TicketObject: "ticketobj", Ref: ref, Scope: "src/",
	})
	if err != nil {
		t.Fatalf("ObservePromotion: %v", err)
	}
	return obs.Agreed, obs.TopAttempt, obs.PromotedAttempt
}

// Test99f_AgreementSurvivesAMerge is §9.9f: when varvig lands a promotion by
// merging the top candidate against a moved base, the promoted change's id
// differs from the candidate's, but the agreement metric must still record
// agreement — the scorer picked the change that was promoted, a merge and all.
func Test99f_AgreementSurvivesAMerge(t *testing.T) {
	p, f, ticket, cand := observeFixture(t)
	const ref, mergeChange = "refs/heads/main", "merge00000000000000000000000000000000000000000000000000000merge"

	// The ref was advanced to a merge change whose parents are the old tip and
	// the candidate — exactly the shape core's landByMerge commits.
	if err := f.UpdateRef(ref, mergeChange, ""); err != nil {
		t.Fatal(err)
	}
	f.SetChange(mergeChange, "", "oldtip000000000000000000000000000000000000000000000000000000old", cand)

	agreed, top, _ := observe(t, p, ticket, ref)
	if top != cand {
		t.Fatalf("top = %q; want the scored candidate %q", top, cand)
	}
	if !agreed {
		t.Fatal("a merge of the top candidate was recorded as disagreement")
	}
	if !f.Called("inspect-change") {
		t.Fatal("the promoted change was never inspected; agreement was decided on the raw id")
	}
}

// Test99f_DirectPromotionAgrees keeps the simple case working: when the ref was
// advanced straight to the candidate, agreement holds without inspecting
// ancestry at all.
func Test99f_DirectPromotionAgrees(t *testing.T) {
	p, f, ticket, cand := observeFixture(t)
	const ref = "refs/heads/main"
	if err := f.UpdateRef(ref, cand, ""); err != nil {
		t.Fatal(err)
	}
	if agreed, _, _ := observe(t, p, ticket, ref); !agreed {
		t.Fatal("a direct promotion of the top candidate was not agreement")
	}
}

// Test99f_UnrelatedPromotionStillDisagrees is the guard against softening: a
// promotion that is neither the top candidate nor a merge carrying it forward
// must still read as disagreement, or the metric would stop measuring anything.
func Test99f_UnrelatedPromotionStillDisagrees(t *testing.T) {
	p, f, ticket, _ := observeFixture(t)
	const ref, other = "refs/heads/main", "other00000000000000000000000000000000000000000000000000000other"
	if err := f.UpdateRef(ref, other, ""); err != nil {
		t.Fatal(err)
	}
	// A merge, but of some other candidate — the top candidate is not a parent.
	f.SetChange(other, "", "oldtip000000000000000000000000000000000000000000000000000000old",
		"xcand00000000000000000000000000000000000000000000000000000xcand")

	if agreed, _, _ := observe(t, p, ticket, ref); agreed {
		t.Fatal("a promotion unrelated to the top candidate was recorded as agreement")
	}
}
