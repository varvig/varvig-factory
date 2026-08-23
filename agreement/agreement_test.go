package agreement

import (
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/varvigcli"
)

var (
	now       = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ticketID  = "a1c0ffee0000000000000000000000000000000000000000000000000000ffee"
	ticketObj = "b2dec0de0000000000000000000000000000000000000000000000000000c0de"
)

func TestObserveComparesExactIdentity(t *testing.T) {
	// Agreement is exact identity of the change, not a similarity judgement:
	// "close enough" would let a scorer look calibrated while consistently
	// ranking second-best first.
	agreed := Observe("src/", ticketID, "abc", "abc", now)
	if !agreed.Agreed {
		t.Fatal("identical top and promoted did not agree")
	}
	disagreed := Observe("src/", ticketID, "abc", "abd", now)
	if disagreed.Agreed {
		t.Fatal("different changes agreed")
	}
	// An empty top is not agreement with an empty promoted.
	if Observe("src/", ticketID, "", "", now).Agreed {
		t.Fatal("two empty attempts agreed")
	}
}

func TestValidateRejectsAnObservationThatCannotMeanAnything(t *testing.T) {
	// No scope: the rate is per-module, never global (§6.4).
	if err := (Observation{PromotedAttempt: "abc", TopAttempt: "abc"}).Validate(); err == nil {
		t.Fatal("an observation with no scope validated")
	}
	// No top-scoring attempt: an unscored pool is not a disagreement, and
	// recording it as one would drag the rate down for a reason that has nothing
	// to do with the scorer.
	err := (Observation{Scope: "src/", PromotedAttempt: "abc"}).Validate()
	if err == nil {
		t.Fatal("an observation with no top attempt validated")
	}
	if !strings.Contains(err.Error(), "unscored pool") {
		t.Fatalf("the error does not explain why: %v", err)
	}
	if err := (Observation{Scope: "src/", TopAttempt: "abc"}).Validate(); err == nil {
		t.Fatal("an observation with no promoted attempt validated")
	}
}

func TestRateValueTreatsAnEmptySampleAsZero(t *testing.T) {
	// No evidence of agreement is not evidence of agreement — the empty sample
	// must not read as 100%, or a fresh scope would unlock autonomy.
	if got := (Rate{Scope: "src/"}).Value(); got != 0 {
		t.Fatalf("empty rate = %g, want 0", got)
	}
	if got := (Rate{Observations: 4, Agreements: 3}).Value(); got != 0.75 {
		t.Fatalf("rate = %g, want 0.75", got)
	}
}

func TestTallyDoesNotRollScopesUp(t *testing.T) {
	// "src/" and "src/generated/" are different modules with different risk.
	// Averaging them is how a good record in one place licenses autonomy in
	// another (§6.4: per-module, not global).
	obs := []Observation{
		{Scope: "src/", Agreed: true, TopAttempt: "a", PromotedAttempt: "a"},
		{Scope: "src/", Agreed: true, TopAttempt: "a", PromotedAttempt: "a"},
		{Scope: "src/generated/", Agreed: false, TopAttempt: "a", PromotedAttempt: "b"},
	}
	rates := Tally(obs)
	if len(rates) != 2 {
		t.Fatalf("tally produced %d scopes, want 2", len(rates))
	}
	if rates["src/"].Value() != 1 {
		t.Fatalf("src/ rate = %g, want 1", rates["src/"].Value())
	}
	if rates["src/generated/"].Value() != 0 {
		t.Fatalf("src/generated/ rate = %g, want 0", rates["src/generated/"].Value())
	}
}

func TestRecordAndReadBackThroughVarvig(t *testing.T) {
	v := varvigcli.NewFake("a")
	v.AddTicket(ticketID, "spec", varvigcli.Scope{Reads: []string{"src"}}, "approved")
	cur, err := v.ResolveRef("refs/varvig/tickets/" + ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.UpdateRef("refs/varvig/tickets/"+ticketID, ticketObj, cur); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		promoted := "top"
		if i >= 3 {
			promoted = "other"
		}
		obs := Observe("src/", ticketID, "top", promoted, now.Add(time.Duration(i)*time.Second))
		if err := Record(v, ticketObj, obs); err != nil {
			t.Fatal(err)
		}
	}

	rate, err := RateFor(v, "src/")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Observations != 5 || rate.Agreements != 3 {
		t.Fatalf("rate = %+v, want 3/5", rate)
	}
	// A scope with nothing recorded reads as zero observations rather than an
	// error: "not measured yet" is the normal starting state.
	empty, err := RateFor(v, "docs/")
	if err != nil {
		t.Fatal(err)
	}
	if empty.Observations != 0 || empty.Scope != "docs/" {
		t.Fatalf("unmeasured scope = %+v", empty)
	}
}

func TestRecordRefusesAnInvalidObservation(t *testing.T) {
	v := varvigcli.NewFake("a")
	if err := Record(v, ticketObj, Observation{Scope: "src/"}); err == nil {
		t.Fatal("an invalid observation was written")
	}
}

func TestObservationsSkipMalformedNotesRatherThanFailing(t *testing.T) {
	// A peer running a newer Factory may write a shape this build does not know.
	// Refusing to compute any rate because one note is unfamiliar would make the
	// metric fragile across versions — and the metric is what licenses autonomy.
	v := varvigcli.NewFake("a")
	v.AddTicket(ticketID, "spec", varvigcli.Scope{Reads: []string{"src"}}, "approved")
	target, err := v.ResolveRef("refs/varvig/tickets/" + ticketID)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.AddNote(target, "factory/agreement", []byte(`{"not":"an observation"}`)); err != nil {
		t.Fatal(err)
	}
	if err := v.AddNote(target, "factory/agreement", []byte(`{{{ broken`)); err != nil {
		t.Fatal(err)
	}
	good := Observe("src/", ticketID, "top", "top", now)
	if err := Record(v, target, good); err != nil {
		t.Fatal(err)
	}

	obs, err := Observations(v)
	if err != nil {
		t.Fatalf("one malformed note broke the whole read: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("read %d observations, want 1 (the valid one)", len(obs))
	}
}

func TestReportIsWorstFirst(t *testing.T) {
	// The scope an operator needs to look at goes on the first line.
	rates := map[string]Rate{
		"good/": {Scope: "good/", Observations: 10, Agreements: 10},
		"bad/":  {Scope: "bad/", Observations: 10, Agreements: 2},
		"mid/":  {Scope: "mid/", Observations: 10, Agreements: 6},
	}
	report := Report(rates)
	if len(report) != 3 {
		t.Fatalf("report has %d rows", len(report))
	}
	if report[0].Scope != "bad/" || report[2].Scope != "good/" {
		t.Fatalf("report is not worst-first: %v", report)
	}
}

func TestGateDefaults(t *testing.T) {
	g := NewGate(0, 0)
	if g.Threshold != DefaultThreshold {
		t.Fatalf("threshold = %g, want %g", g.Threshold, DefaultThreshold)
	}
	if g.MinObservations != DefaultMinObservations {
		t.Fatalf("min observations = %d, want %d", g.MinObservations, DefaultMinObservations)
	}
	// An explicit configuration wins, so a deployment can be stricter.
	strict := NewGate(0.95, 100)
	if strict.Threshold != 0.95 || strict.MinObservations != 100 {
		t.Fatalf("explicit gate = %+v", strict)
	}
}
