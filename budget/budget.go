// Package budget declares and enforces a cell's spend cap (FACTORY.md §7).
//
// This is not optional and it is not advisory. Attempts multiply cost, and a
// disconnected cell claiming speculatively can burn budget on work that proves
// duplicative. The rule the whole package exists to enforce is one sentence:
//
//	Halt, do not degrade.
//
// A cell out of budget stops claiming and says so. It does not switch to a
// smaller model, shorten its context, or reduce its attempt count, because all
// three produce attempts that pollute selection — and the pollution is invisible
// in the output. It shows up weeks later as a selection statistic nobody can
// explain.
package budget

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Budget is a cell's declared caps (CELL.md §8).
//
// The unit of InferenceDaily is deliberately unnamed: it is whatever unit the
// operator prices inference in — currency, or tokens, or credits. Factory does
// not convert between them, because a conversion rate is a fact about a contract
// and would go stale silently.
type Budget struct {
	// InferenceDaily is the total spend allowed per UTC day. Zero means no
	// inference is permitted at all, which is the correct default for a cell
	// with no model: a verify/build cell should not have an inference budget it
	// could only spend by being misconfigured.
	InferenceDaily float64 `json:"inference_daily"`
	// VerifyConcurrent caps simultaneous verification jobs.
	VerifyConcurrent int `json:"verify_concurrent"`
	// StorageGB caps the cell-local artifact store.
	StorageGB float64 `json:"storage_gb"`
	// AttemptsDefault is how many attempts a claim asks varvig for when the
	// ticket does not say.
	AttemptsDefault int `json:"attempts_default"`
	// OfflineInferenceDaily caps spend while upstream is unreachable, and is
	// capped separately and more tightly (§7): a disconnected cell cannot check
	// whether another cell already succeeded, so every offline unit is spent
	// without knowing whether the work is duplicative.
	//
	// Zero means "derive it": DefaultOfflineShare of the daily cap.
	OfflineInferenceDaily float64 `json:"offline_inference_daily,omitempty"`
	// PerCallCost prices a call whose runtime reported no usage — a CLI
	// runtime, typically. Without it such a call would be free in the ledger,
	// and a cell driving a local binary would have no cap at all.
	PerCallCost float64 `json:"per_call_cost,omitempty"`
	// CostPerKTokenIn and CostPerKTokenOut price a call the runtime did report
	// usage for.
	CostPerKTokenIn  float64 `json:"cost_per_ktoken_in,omitempty"`
	CostPerKTokenOut float64 `json:"cost_per_ktoken_out,omitempty"`
}

// DefaultOfflineShare is the fraction of the daily cap available while
// disconnected, when the operator has not set an explicit offline cap. A
// quarter, because offline spend is the least informed spend a cell does: it
// cannot see another cell's success, so it is the spend most likely to be
// duplicative.
const DefaultOfflineShare = 0.25

// OfflineCap is the effective offline cap.
func (b Budget) OfflineCap() float64 {
	if b.OfflineInferenceDaily > 0 {
		return min(b.OfflineInferenceDaily, b.InferenceDaily)
	}
	return b.InferenceDaily * DefaultOfflineShare
}

// Attempts is how many attempts to request, honouring a per-ticket override.
func (b Budget) Attempts(override int) int {
	if override > 0 {
		return override
	}
	if b.AttemptsDefault > 0 {
		return b.AttemptsDefault
	}
	return 1
}

// StorageBytes is the storage cap in bytes.
func (b Budget) StorageBytes() int64 { return int64(b.StorageGB * 1024 * 1024 * 1024) }

// Validate rejects a budget that cannot be enforced.
func (b Budget) Validate() error {
	if b.InferenceDaily < 0 {
		return fmt.Errorf("budget: inference_daily is negative")
	}
	if b.StorageGB < 0 {
		return fmt.Errorf("budget: storage_gb is negative")
	}
	if b.VerifyConcurrent < 0 {
		return fmt.Errorf("budget: verify_concurrent is negative")
	}
	if b.AttemptsDefault < 0 {
		return fmt.Errorf("budget: attempts_default is negative")
	}
	if b.OfflineInferenceDaily > b.InferenceDaily {
		// A looser offline cap than the online one inverts the §7 rule. It is
		// almost certainly a typo, and left in place it would make the least
		// informed spend the least constrained.
		return fmt.Errorf("budget: offline_inference_daily (%g) exceeds inference_daily (%g); offline spend is capped more tightly, not less",
			b.OfflineInferenceDaily, b.InferenceDaily)
	}
	// A cell that can spend but cannot price what it spends has no cap. Catch
	// it at startup rather than after a day of free calls.
	if b.InferenceDaily > 0 && b.PerCallCost == 0 && b.CostPerKTokenIn == 0 && b.CostPerKTokenOut == 0 {
		return fmt.Errorf("budget: inference_daily is set but no price is: set per_call_cost, or cost_per_ktoken_in/out, or the cap cannot be enforced")
	}
	return nil
}

// Price converts one inference call's reported usage into ledger units. A call
// the runtime reported no usage for is priced at PerCallCost, which is why that
// field exists: the alternative is a call that costs nothing and a cap that
// therefore does nothing.
func (b Budget) Price(tokensIn, tokensOut int) float64 {
	if tokensIn == 0 && tokensOut == 0 {
		return b.PerCallCost
	}
	cost := float64(tokensIn)/1000*b.CostPerKTokenIn + float64(tokensOut)/1000*b.CostPerKTokenOut
	if cost == 0 {
		// Usage was reported but no per-token price is configured. Falling back
		// to the per-call price is better than charging zero: an
		// under-approximation still moves the ledger, a zero never halts.
		return b.PerCallCost
	}
	return cost
}

// Reason is why a cell may not proceed.
type Reason string

// The halt reasons. Each is reported by name, because "out of budget" without
// which budget sends an operator to read the source.
const (
	ReasonNone            Reason = ""
	ReasonInferenceDaily  Reason = "daily inference cap reached"
	ReasonOfflineCap      Reason = "offline inference cap reached"
	ReasonStorage         Reason = "storage cap reached"
	ReasonVerifySaturated Reason = "verification slots saturated"
	ReasonNoInference     Reason = "no inference budget declared"
)

// Decision is the ledger's answer to "may I spend?".
type Decision struct {
	OK     bool
	Reason Reason
	// Spent and Cap are the numbers behind the decision, so a refusal can be
	// reported rather than merely returned.
	Spent, Cap float64
}

// Error renders a refusal for a human. A halting cell must say so (§7).
func (d Decision) String() string {
	if d.OK {
		return "ok"
	}
	if d.Cap > 0 {
		return fmt.Sprintf("halted: %s (%.4g of %.4g spent)", d.Reason, d.Spent, d.Cap)
	}
	return "halted: " + string(d.Reason)
}

// Ledger tracks spend against a budget and persists it, so a restart does not
// reset the cap. A cell whose daily cap resets on every crash has no daily cap.
type Ledger struct {
	mu     sync.Mutex
	budget Budget
	path   string

	// day is the UTC date the current totals belong to, as "2006-01-02". The
	// window is a calendar day rather than a rolling 24 hours because an
	// operator reasons about days, and a rolling window makes "how much is left
	// today" unanswerable.
	day            string
	spent          float64
	offlineSpent   float64
	calls          int
	verifyInFlight int
}

// state is the persisted form.
type state struct {
	Day          string  `json:"day"`
	Spent        float64 `json:"spent"`
	OfflineSpent float64 `json:"offline_spent"`
	Calls        int     `json:"calls"`
}

// NewLedger opens or creates a ledger at path. An empty path keeps it in memory,
// which is what tests and a one-shot run want.
func NewLedger(b Budget, path string, now time.Time) (*Ledger, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	l := &Ledger{budget: b, path: path, day: utcDay(now)}
	if path == "" {
		return l, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, err
	}
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		// A corrupt ledger must not read as an empty one: that would hand the
		// cell a fresh cap. Refusing to start is the safe failure.
		return nil, fmt.Errorf("budget: ledger at %s is unreadable: %w", path, err)
	}
	if s.Day == l.day {
		l.spent, l.offlineSpent, l.calls = s.Spent, s.OfflineSpent, s.Calls
	}
	return l, nil
}

// Budget returns the declared budget.
func (l *Ledger) Budget() Budget { return l.budget }

// rollover resets the totals when the UTC day has changed. Called with the lock
// held.
func (l *Ledger) rollover(now time.Time) {
	if d := utcDay(now); d != l.day {
		l.day, l.spent, l.offlineSpent, l.calls = d, 0, 0, 0
	}
}

// CanSpend reports whether the cell may make an inference call now. offline says
// whether upstream is currently unreachable, which selects the tighter cap.
//
// It answers before the call, not after: a cell that checked afterwards would
// always overshoot by one attempt, and an attempt is the expensive unit.
func (l *Ledger) CanSpend(now time.Time, offline bool) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover(now)

	if l.budget.InferenceDaily <= 0 {
		return Decision{Reason: ReasonNoInference}
	}
	if l.spent >= l.budget.InferenceDaily {
		return Decision{Reason: ReasonInferenceDaily, Spent: l.spent, Cap: l.budget.InferenceDaily}
	}
	if offline {
		if limit := l.budget.OfflineCap(); l.offlineSpent >= limit {
			return Decision{Reason: ReasonOfflineCap, Spent: l.offlineSpent, Cap: limit}
		}
	}
	return Decision{OK: true, Spent: l.spent, Cap: l.budget.InferenceDaily}
}

// Spend records a completed inference call. It returns the cost recorded, so a
// caller can log what an attempt actually cost rather than what it estimated.
func (l *Ledger) Spend(now time.Time, offline bool, tokensIn, tokensOut int) float64 {
	cost := l.budget.Price(tokensIn, tokensOut)
	l.mu.Lock()
	l.rollover(now)
	l.spent += cost
	l.calls++
	if offline {
		l.offlineSpent += cost
	}
	l.mu.Unlock()
	// A persist failure must not silently lose the record, but it also must not
	// abort work already paid for. Report it and continue: the in-memory total
	// is still correct for this process.
	_ = l.persist()
	return cost
}

// Snapshot is the ledger's current state, for reporting.
type Snapshot struct {
	Day          string
	Spent        float64
	OfflineSpent float64
	Calls        int
	Cap          float64
	OfflineCap   float64
}

// Snapshot returns the current totals.
func (l *Ledger) Snapshot(now time.Time) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover(now)
	return Snapshot{
		Day:          l.day,
		Spent:        l.spent,
		OfflineSpent: l.offlineSpent,
		Calls:        l.calls,
		Cap:          l.budget.InferenceDaily,
		OfflineCap:   l.budget.OfflineCap(),
	}
}

// AcquireVerify takes a verification slot, or refuses. Slots are a concurrency
// cap rather than a spend cap: verification is cheap but not free, and a Micro
// cell with four cores that runs twelve suites at once is slower than one that
// runs four.
func (l *Ledger) AcquireVerify() Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.budget.VerifyConcurrent
	if limit <= 0 {
		limit = 1
	}
	if l.verifyInFlight >= limit {
		return Decision{Reason: ReasonVerifySaturated, Spent: float64(l.verifyInFlight), Cap: float64(limit)}
	}
	l.verifyInFlight++
	return Decision{OK: true}
}

// ReleaseVerify returns a slot.
func (l *Ledger) ReleaseVerify() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.verifyInFlight > 0 {
		l.verifyInFlight--
	}
}

func (l *Ledger) persist() error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	s := state{Day: l.day, Spent: l.spent, OfflineSpent: l.offlineSpent, Calls: l.calls}
	path := l.path
	l.mu.Unlock()

	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Releaser releases cell-local artifacts to relieve storage pressure. It is the
// artifact store's List/Release pair, narrowed to what StoragePressure needs.
type Releaser interface {
	UsedBytes() (int64, error)
	// Candidates are the artifacts that may be released, in the order they
	// should be released — largest first, so the fewest retention obligations
	// are dropped for the most disk freed.
	Candidates() ([]string, error)
	// Release drops one artifact's local copy and its pin, in that order.
	Release(contentHash string) error
}

// StorageRelief is what a pressure pass did.
type StorageRelief struct {
	Before, After, Cap int64
	Released           []string
	// StillOver reports that the pass could not get under the cap. The cell
	// stops claiming rather than continuing to write: at that point the next
	// artifact is the one that fills the disk.
	StillOver bool
}

// RelieveStoragePressure releases pins and local artifacts until the store is
// under its cap (FACTORY.md §7).
//
// The order matters and is the whole point of doing this here rather than
// letting a disk fill: a cell drops its own retention obligations *deliberately*
// — releasing the pin first, then the bytes — rather than collecting state
// another cell is still evaluating and then dying. A cell that ran out of disk
// while holding a pin has silently promised to retain something it no longer
// has.
func RelieveStoragePressure(b Budget, r Releaser) (StorageRelief, error) {
	limit := b.StorageBytes()
	used, err := r.UsedBytes()
	if err != nil {
		return StorageRelief{}, err
	}
	relief := StorageRelief{Before: used, After: used, Cap: limit}
	if limit <= 0 || used <= limit {
		return relief, nil
	}
	candidates, err := r.Candidates()
	if err != nil {
		return relief, err
	}
	for _, c := range candidates {
		if relief.After <= limit {
			break
		}
		if err := r.Release(c); err != nil {
			return relief, fmt.Errorf("budget: releasing %s: %w", c, err)
		}
		relief.Released = append(relief.Released, c)
		after, err := r.UsedBytes()
		if err != nil {
			return relief, err
		}
		relief.After = after
	}
	relief.StillOver = relief.After > limit
	return relief, nil
}

// SortedReasons is every halt reason, for documentation and for a CLI that lists
// them. Sorted so the output is stable.
func SortedReasons() []Reason {
	out := []Reason{
		ReasonInferenceDaily, ReasonNoInference, ReasonOfflineCap,
		ReasonStorage, ReasonVerifySaturated,
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
