package budget

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var day0 = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func priced(daily float64) Budget {
	return Budget{InferenceDaily: daily, PerCallCost: 1, AttemptsDefault: 3, VerifyConcurrent: 2}
}

// TestBudgetHalts is FACTORY.md §9.6: the cell stops claiming at the cap. The
// companion half — that it does not silently downgrade — is asserted in the loop
// package, where a model choice exists to downgrade.
func TestBudgetHalts(t *testing.T) {
	l, err := NewLedger(priced(3), "", day0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if d := l.CanSpend(day0, false); !d.OK {
			t.Fatalf("refused spend %d of 3: %s", i+1, d)
		}
		l.Spend(day0, false, 0, 0)
	}
	d := l.CanSpend(day0, false)
	if d.OK {
		t.Fatal("the cell kept spending past its cap")
	}
	if d.Reason != ReasonInferenceDaily {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInferenceDaily)
	}
	// A halting cell must say so, with the numbers (§7).
	if !strings.Contains(d.String(), "3") {
		t.Fatalf("refusal does not report the numbers: %s", d)
	}
}

func TestOfflineCapIsTighterAndSeparate(t *testing.T) {
	// §7: speculative claiming while offline is capped separately and more
	// tightly, because a disconnected cell cannot check whether another cell
	// already succeeded.
	b := priced(10)
	b.OfflineInferenceDaily = 2
	l, err := NewLedger(b, "", day0)
	if err != nil {
		t.Fatal(err)
	}
	l.Spend(day0, true, 0, 0)
	l.Spend(day0, true, 0, 0)

	if d := l.CanSpend(day0, true); d.OK {
		t.Fatal("offline spend continued past the offline cap")
	} else if d.Reason != ReasonOfflineCap {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonOfflineCap)
	}
	// The online cap is untouched: reconnecting must let the cell work again,
	// otherwise an offline burst would silently cost the rest of the day.
	if d := l.CanSpend(day0, false); !d.OK {
		t.Fatalf("online spend was refused after the offline cap: %s", d)
	}
}

func TestOfflineCapDefaultsToAShareOfTheDailyCap(t *testing.T) {
	l, err := NewLedger(priced(100), "", day0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.Budget().OfflineCap(), 25.0; got != want {
		t.Fatalf("default offline cap = %g, want %g", got, want)
	}
}

func TestValidateRejectsALooserOfflineCap(t *testing.T) {
	// A looser offline cap inverts the §7 rule, so it is a startup error rather
	// than a configuration that quietly makes the least informed spend the
	// least constrained.
	b := priced(10)
	b.OfflineInferenceDaily = 20
	if err := b.Validate(); err == nil {
		t.Fatal("an offline cap looser than the online cap was accepted")
	}
}

func TestValidateRejectsAnUnpriceableBudget(t *testing.T) {
	// A cell that can spend but cannot price what it spends has no cap at all.
	if err := (Budget{InferenceDaily: 50}).Validate(); err == nil {
		t.Fatal("a budget with a cap but no price was accepted")
	}
	// Either pricing form is enough.
	if err := (Budget{InferenceDaily: 50, CostPerKTokenOut: 0.01}).Validate(); err != nil {
		t.Fatalf("a token-priced budget was rejected: %v", err)
	}
	// A cell with no inference budget needs no price: that is a verify/build
	// cell, and it is the common case.
	if err := (Budget{StorageGB: 20}).Validate(); err != nil {
		t.Fatalf("a model-less cell's budget was rejected: %v", err)
	}
}

func TestNoInferenceBudgetRefusesByName(t *testing.T) {
	// A Micro cell with roles verify+build has no inference budget, and an
	// attempt attempted anyway must fail with a reason an operator can act on.
	l, err := NewLedger(Budget{StorageGB: 10}, "", day0)
	if err != nil {
		t.Fatal(err)
	}
	d := l.CanSpend(day0, false)
	if d.OK || d.Reason != ReasonNoInference {
		t.Fatalf("decision = %+v, want a %q refusal", d, ReasonNoInference)
	}
}

func TestPriceUsesTokensWhenReportedAndPerCallWhenNot(t *testing.T) {
	b := Budget{InferenceDaily: 100, PerCallCost: 0.5, CostPerKTokenIn: 1, CostPerKTokenOut: 2}
	if got, want := b.Price(1000, 500), 1.0+1.0; got != want {
		t.Fatalf("token price = %g, want %g", got, want)
	}
	// A CLI runtime reports nothing; that call must still cost something, or a
	// cell driving a local binary has no cap.
	if got, want := b.Price(0, 0), 0.5; got != want {
		t.Fatalf("per-call price = %g, want %g", got, want)
	}
	// Usage reported but no per-token price configured: fall back rather than
	// charge zero. An under-approximation still moves the ledger; a zero never
	// halts.
	noTokenPrice := Budget{InferenceDaily: 10, PerCallCost: 0.25}
	if got, want := noTokenPrice.Price(1000, 1000), 0.25; got != want {
		t.Fatalf("fallback price = %g, want %g", got, want)
	}
}

func TestDayRollsOverAndPersistenceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := NewLedger(priced(3), path, day0)
	if err != nil {
		t.Fatal(err)
	}
	l.Spend(day0, false, 0, 0)
	l.Spend(day0, false, 0, 0)
	l.Spend(day0, false, 0, 0)
	if l.CanSpend(day0, false).OK {
		t.Fatal("cap not reached")
	}

	// A restart must not hand the cell a fresh cap: a daily cap that resets on
	// every crash is not a daily cap.
	reopened, err := NewLedger(priced(3), path, day0)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CanSpend(day0, false).OK {
		t.Fatal("restarting reset the daily cap")
	}
	if got := reopened.Snapshot(day0).Spent; got != 3 {
		t.Fatalf("restored spend = %g, want 3", got)
	}

	// The next UTC day starts fresh.
	nextDay := day0.Add(24 * time.Hour)
	if d := reopened.CanSpend(nextDay, false); !d.OK {
		t.Fatalf("the cap did not roll over: %s", d)
	}
	if got := reopened.Snapshot(nextDay).Spent; got != 0 {
		t.Fatalf("spend after rollover = %g, want 0", got)
	}
}

func TestACorruptLedgerRefusesToStart(t *testing.T) {
	// A corrupt ledger read as an empty one would hand the cell a fresh cap.
	// Refusing to start is the safe failure.
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLedger(priced(3), path, day0); err == nil {
		t.Fatal("a corrupt ledger was treated as empty")
	}
}

func TestVerifySlotsAreBounded(t *testing.T) {
	l, err := NewLedger(priced(10), "", day0)
	if err != nil {
		t.Fatal(err)
	}
	if !l.AcquireVerify().OK || !l.AcquireVerify().OK {
		t.Fatal("could not take the two configured slots")
	}
	d := l.AcquireVerify()
	if d.OK || d.Reason != ReasonVerifySaturated {
		t.Fatalf("decision = %+v, want saturated", d)
	}
	l.ReleaseVerify()
	if !l.AcquireVerify().OK {
		t.Fatal("a released slot was not reusable")
	}
	// Over-releasing must not manufacture slots.
	for i := 0; i < 5; i++ {
		l.ReleaseVerify()
	}
	if !l.AcquireVerify().OK || !l.AcquireVerify().OK {
		t.Fatal("slots unavailable after over-release")
	}
	if l.AcquireVerify().OK {
		t.Fatal("over-releasing manufactured a slot")
	}
}

func TestAttemptsHonoursOverrideThenDefault(t *testing.T) {
	b := priced(10)
	if got := b.Attempts(0); got != 3 {
		t.Fatalf("attempts = %d, want the declared default 3", got)
	}
	if got := b.Attempts(5); got != 5 {
		t.Fatalf("attempts = %d, want the override 5", got)
	}
	// Never zero: an attempts count of zero is a cell that claims and does
	// nothing, which looks like a hung cell.
	if got := (Budget{}).Attempts(0); got != 1 {
		t.Fatalf("attempts with nothing declared = %d, want 1", got)
	}
}

// fakeReleaser is a Releaser over an in-memory size map.
type fakeReleaser struct {
	sizes    map[string]int64
	order    []string
	released []string
	// pinReleasedBefore records, per artifact, whether its pin was dropped
	// before its bytes were. The order is the §7 requirement.
	pinFirst map[string]bool
	pinsHeld map[string]bool
}

func (f *fakeReleaser) UsedBytes() (int64, error) {
	var total int64
	for _, s := range f.sizes {
		total += s
	}
	return total, nil
}

func (f *fakeReleaser) Candidates() ([]string, error) { return f.order, nil }

func (f *fakeReleaser) Release(hash string) error {
	// A real releaser drops the pin, then the bytes. Model both so the ordering
	// is observable.
	f.pinFirst[hash] = f.pinsHeld[hash]
	delete(f.pinsHeld, hash)
	delete(f.sizes, hash)
	f.released = append(f.released, hash)
	return nil
}

func TestStoragePressureReleasesUntilUnderCap(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	r := &fakeReleaser{
		sizes:    map[string]int64{"big": 3 * gb, "mid": 2 * gb, "small": 1 * gb},
		order:    []string{"big", "mid", "small"}, // largest first
		pinFirst: map[string]bool{},
		pinsHeld: map[string]bool{"big": true, "mid": true, "small": true},
	}
	relief, err := RelieveStoragePressure(Budget{StorageGB: 3}, r)
	if err != nil {
		t.Fatal(err)
	}
	if relief.StillOver {
		t.Fatalf("still over cap after relief: %+v", relief)
	}
	// Largest first means one release sufficed: 3 GB freed brings 6 GB to 3 GB.
	if len(relief.Released) != 1 || relief.Released[0] != "big" {
		t.Fatalf("released %v, want just the largest", relief.Released)
	}
	// The pin was dropped before the bytes: a cell that ran out of disk while
	// holding a pin has silently promised to retain something it no longer has.
	if !r.pinFirst["big"] {
		t.Fatal("bytes were released without dropping the retention obligation first")
	}
}

func TestStoragePressureReportsWhenItCannotGetUnderCap(t *testing.T) {
	gb := int64(1024 * 1024 * 1024)
	r := &fakeReleaser{
		sizes:    map[string]int64{"only": 10 * gb},
		order:    nil, // nothing releasable: everything is still being evaluated
		pinFirst: map[string]bool{},
		pinsHeld: map[string]bool{},
	}
	relief, err := RelieveStoragePressure(Budget{StorageGB: 1}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !relief.StillOver {
		t.Fatal("a cell that could not free enough disk reported success")
	}
}

func TestStoragePressureIsANoOpUnderCap(t *testing.T) {
	r := &fakeReleaser{sizes: map[string]int64{"a": 10}, pinFirst: map[string]bool{}, pinsHeld: map[string]bool{}}
	relief, err := RelieveStoragePressure(Budget{StorageGB: 100}, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(relief.Released) != 0 {
		t.Fatalf("released %v while under cap", relief.Released)
	}
	// No cap declared means no pressure relief: an operator who has not set a
	// storage cap has not asked for artifacts to be deleted.
	unbounded, err := RelieveStoragePressure(Budget{}, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded.Released) != 0 {
		t.Fatalf("released %v with no cap declared", unbounded.Released)
	}
}
