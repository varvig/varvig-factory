package budget

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/cell"
)

var day0 = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func priced(daily cell.Money) Budget {
	return Budget{InferenceDaily: daily, PerCallCost: 100, AttemptsDefault: 3, VerifyConcurrent: 2}
}

// TestBudgetHalts is FACTORY.md §9.6: the cell stops claiming at the cap. The
// companion half — that it does not silently downgrade — is asserted in the loop
// package, where a model choice exists to downgrade.
func TestBudgetHalts(t *testing.T) {
	l, err := NewLedger(priced(300), "", day0)
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
	b := priced(1000)
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
	l, err := NewLedger(priced(10000), "", day0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.Budget().OfflineCap(), cell.Money(2500); got != want {
		t.Fatalf("default offline cap = %s, want %s", got, want)
	}
}

func TestValidateRejectsALooserOfflineCap(t *testing.T) {
	// A looser offline cap inverts the §7 rule, so it is a startup error rather
	// than a configuration that quietly makes the least informed spend the
	// least constrained.
	b := priced(1000)
	b.OfflineInferenceDaily = 2000
	if err := b.Validate(); err == nil {
		t.Fatal("an offline cap looser than the online cap was accepted")
	}
}

func TestValidateRejectsAnUnpriceableBudget(t *testing.T) {
	// A cell that can spend but cannot price what it spends has no cap at all.
	if err := (Budget{InferenceDaily: 5000}).Validate(); err == nil {
		t.Fatal("a budget with a cap but no price was accepted")
	}
	// Either pricing form is enough.
	if err := (Budget{InferenceDaily: 5000, CostPerKTokenOut: 1}).Validate(); err != nil {
		t.Fatalf("a token-priced budget was rejected: %v", err)
	}
	// A cell with no inference budget needs no price: that is a verify/build
	// cell, and it is the common case.
	if err := (Budget{StorageGB: 20}).Validate(); err != nil {
		t.Fatalf("a model-less cell's budget was rejected: %v", err)
	}
}

func TestAnUnconfiguredBudgetIsUnenforcedNotZero(t *testing.T) {
	// §7.0, and this test used to assert the opposite. An unset inference cap
	// meant "no inference budget declared" and refused every spend, which made
	// a factory that had configured nothing refuse to attempt anything —
	// broken rather than safe. Absence means no enforcement.
	//
	// Both an entirely empty budget and one that configures unrelated things
	// have to pass, because "I set a storage cap" is not a statement about
	// executor.
	for _, b := range []Budget{{}, {StorageGB: 10}} {
		l, err := NewLedger(b, "", day0)
		if err != nil {
			t.Fatalf("budget %+v was rejected: %v", b, err)
		}
		if d := l.CanSpend(day0, false); !d.OK {
			t.Fatalf("budget %+v: online spend refused as %q; unconfigured means unenforced", b, d.Reason)
		}
		// Offline too: the offline cap is a fraction of the daily one, and a
		// fraction of unlimited is unlimited, not zero.
		if d := l.CanSpend(day0, true); !d.OK {
			t.Fatalf("budget %+v: offline spend refused as %q", b, d.Reason)
		}
	}
}

func TestAConfiguredCapStillHalts(t *testing.T) {
	// The other half of the pair: making absence permissive must not make
	// presence permissive. A cell that set a cap still stops at it.
	l, err := NewLedger(Budget{InferenceDaily: 1000, PerCallCost: 600}, "", day0)
	if err != nil {
		t.Fatal(err)
	}
	if d := l.CanSpend(day0, false); !d.OK {
		t.Fatalf("first spend refused: %+v", d)
	}
	l.Spend(day0, false, 0, 0)
	l.Spend(day0, false, 0, 0)
	if d := l.CanSpend(day0, false); d.OK || d.Reason != ReasonInferenceDaily {
		t.Fatalf("decision after exceeding the cap = %+v, want a %q refusal", d, ReasonInferenceDaily)
	}
}

func TestPriceUsesTokensWhenReportedAndPerCallWhenNot(t *testing.T) {
	b := Budget{InferenceDaily: 10000, PerCallCost: 50, CostPerKTokenIn: 100, CostPerKTokenOut: 200}
	// 1000 in at 1.00/k plus 500 out at 2.00/k.
	if got, want := b.Price(1000, 500), cell.Money(100+100); got != want {
		t.Fatalf("token price = %s, want %s", got, want)
	}
	// A CLI runtime reports nothing; that call must still cost something, or a
	// cell driving a local binary has no cap.
	if got, want := b.Price(0, 0), cell.Money(50); got != want {
		t.Fatalf("per-call price = %s, want %s", got, want)
	}
	// Usage reported but no per-token price configured: fall back rather than
	// charge zero. An under-approximation still moves the ledger; a zero never
	// halts.
	noTokenPrice := Budget{InferenceDaily: 1000, PerCallCost: 25}
	if got, want := noTokenPrice.Price(1000, 1000), cell.Money(25); got != want {
		t.Fatalf("fallback price = %s, want %s", got, want)
	}
}

func TestDayRollsOverAndPersistenceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := NewLedger(priced(300), path, day0)
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
	reopened, err := NewLedger(priced(300), path, day0)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CanSpend(day0, false).OK {
		t.Fatal("restarting reset the daily cap")
	}
	if got := reopened.Snapshot(day0).Spent; got != 300 {
		t.Fatalf("restored spend = %s, want 3.00", got)
	}

	// The next UTC day starts fresh.
	nextDay := day0.Add(24 * time.Hour)
	if d := reopened.CanSpend(nextDay, false); !d.OK {
		t.Fatalf("the cap did not roll over: %s", d)
	}
	if got := reopened.Snapshot(nextDay).Spent; got != 0 {
		t.Fatalf("spend after rollover = %s, want 0", got)
	}
}

func TestACorruptLedgerRefusesToStart(t *testing.T) {
	// A corrupt ledger read as an empty one would hand the cell a fresh cap.
	// Refusing to start is the safe failure.
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLedger(priced(300), path, day0); err == nil {
		t.Fatal("a corrupt ledger was treated as empty")
	}
}

func TestVerifySlotsAreBounded(t *testing.T) {
	l, err := NewLedger(priced(1000), "", day0)
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
	b := priced(1000)
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
