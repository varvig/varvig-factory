// Package conformance is FACTORY.md §9: the nine numbered tests, in its order,
// each named for the spec item it holds.
//
// They are collected in one package rather than scattered across the packages
// they exercise because each one is a property of the *cell*, not of a component
// — "both attempts survive reconnect" is not a fact about claims or about sync,
// it is a fact about what happens when you partition two cells. Several of them
// exist, in the spec's own words, "to keep it that way": the behaviour is
// already correct and the test is there so a later refactor cannot quietly make
// it not.
package conformance

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/artifact"
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/gate"
	"github.com/varvig/varvig-factory/inference"
	"github.com/varvig/varvig-factory/loop"
	"github.com/varvig/varvig-factory/profile"
	"github.com/varvig/varvig-factory/promote"
	"github.com/varvig/varvig-factory/sandbox"
	"github.com/varvig/varvig-factory/varvigcli"
)

var (
	now = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// Ids are bare hex, the shape varvig prints: they are interpolated into ref
	// names, and varvig's pin refs accept nothing else.
	taskID   = "a1c0ffee00000000000000000000000000000000000000000000000000000001"
	taskObj  = "b2dec0de00000000000000000000000000000000000000000000000000000002"
	theScope = varvigcli.Scope{Reads: []string{"src"}, Writes: []string{"src"}}
)

// harness is one configured cell plus the fakes behind it.
type harness struct {
	t      *testing.T
	Cell   *loop.Cell
	V      *varvigcli.Fake
	Model  *inference.Fake
	Box    *sandbox.Fake
	Sw     *promote.Switch
	Ledger *budget.Ledger
	Logs   []string
}

type opts struct {
	cellID   string
	roles    []cell.Role
	tier     cell.Tier
	budget   budget.Budget
	upstream *varvigcli.Fake
	// switchPath, when set, backs the promotion switch with a file so the kill
	// switch can be thrown from "outside" the running cell.
	switchPath  string
	fingerprint string
	// reply is what the model returns.
	reply string
	// modelVersion and runtimeVersion let two cells be deliberately different
	// environment classes.
	modelVersion, runtimeVersion string
	platform                     string
	baselines                    map[string]cell.Environment
	failing                      map[string]cell.Status
	noChecks                     bool
}

func defaultOpts(cellID string) opts {
	return opts{
		cellID: cellID,
		roles:  []cell.Role{cell.RoleAttempt, cell.RoleBuild, cell.RoleVerify},
		tier:   cell.TierLarge,
		budget: budget.Budget{
			InferenceDaily: 10, PerCallCost: 1,
			VerifyConcurrent: 2, StorageGB: 10, AttemptsDefault: 1,
		},
		reply: "--- src/a.go\npackage src\n\nfunc A() {}\n",
	}
}

func newHarness(t *testing.T, o opts) *harness {
	t.Helper()
	if o.reply == "" {
		o.reply = defaultOpts(o.cellID).reply
	}
	v := varvigcli.NewFake(o.cellID)
	v.AddTicket(taskID, "Add A to src.\n", theScope, "approved")
	// The ticket's ref resolves to a stable object, which is where agreement
	// observations attach.
	if err := v.UpdateRef("refs/varvig/tickets/"+taskID, taskObj, ""); err != nil {
		// AddTicket already created it with its own hash; overwrite so tests can
		// name the object.
		cur, _ := v.ResolveRef("refs/varvig/tickets/" + taskID)
		if err := v.UpdateRef("refs/varvig/tickets/"+taskID, taskObj, cur); err != nil {
			t.Fatal(err)
		}
	}
	upstreamAddr := ""
	if o.upstream != nil {
		v.Upstream = o.upstream
		// The loop needs an upstream *address* to try, not just a wired peer: a
		// cell with no upstream configured is not offline, it simply has nothing
		// to be disconnected from.
		upstreamAddr = "upstream"
	}
	if o.fingerprint != "" {
		v.SetTrust(varvigcli.TrustEntry{
			Fingerprint: o.fingerprint, Name: o.cellID, Scope: "src/", Rights: []string{"promote"},
		})
	}

	model := &inference.Fake{
		Reply:          o.reply,
		Model:          "test-model",
		Version:        o.modelVersion,
		RuntimeVersion: o.runtimeVersion,
	}
	var runtime inference.Runtime = model
	if o.tier == cell.TierNone {
		runtime = inference.None{}
	}
	box := &sandbox.Fake{Platform: o.platform, Results: o.failing}

	ledger, err := budget.NewLedger(o.budget, "", now)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := promote.NewSwitch(o.switchPath)
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, V: v, Model: model, Box: box, Sw: sw, Ledger: ledger}
	models := []cell.Model{{ID: "test-model", Version: o.modelVersion}}
	if o.tier == cell.TierNone {
		models = nil
	}
	checks := []loop.Check{
		{Name: "build", Command: []string{"true"}, Kind: cell.RoleBuild},
		{Name: "unit", Command: []string{"true"}, Kind: cell.RoleVerify},
	}
	if o.noChecks {
		checks = nil
	}
	h.Cell = &loop.Cell{
		Capabilities: cell.Capabilities{
			CellID:    o.cellID,
			Inference: cell.Inference{Tier: o.tier, Models: models},
			Build:     []string{"go"},
			Test:      []string{"unit"},
			Roles:     o.roles,
		},
		V:         v,
		Inference: runtime,
		Sandbox:   box,
		Artifacts: &artifact.LocalCAS{Root: filepath.Join(t.TempDir(), "cas")},
		Ledger:    ledger,
		Upstream:  upstreamAddr,
		Checks:    checks,
		ClaimTTL:  30 * time.Minute,
		TaskTTL:   time.Hour,
		WorkDir:   t.TempDir(),
		Baselines: o.baselines,
		Now:       func() time.Time { return now },
		Log:       func(s string) { h.Logs = append(h.Logs, s) },
	}
	h.Cell.Promoter = &promote.Promoter{
		V: v, Switch: sw, Gate: gate.Module{V: v},
		Agreement:   agreement.NewGate(0, 0),
		Reverify:    h.Cell,
		CellID:      o.cellID,
		Fingerprint: o.fingerprint,
		Now:         func() time.Time { return now },
		Log:         func(s string) { h.Logs = append(h.Logs, s) },
	}
	return h
}

// allowGate binds a policy module that promotes. Without one, promotion refuses
// on "no module bound" — which is the correct default and would mask every other
// condition under test.
func (h *harness) allowGate() {
	h.V.BindHook(gate.Event, func([]byte) varvigcli.HookResult {
		return varvigcli.HookResult{ExitCode: 0, Stdout: "policy allows"}
	})
}

// recordAgreement writes n agreement observations for a scope, all agreeing, so
// the §6.3 condition-5 gate is satisfied and the condition under test is the one
// that decides.
func (h *harness) recordAgreement(scope string, n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		obs := agreement.Observe(scope, taskID, "aa11top000000000000000000000000000000000000000000000000000000aa11", "aa11top000000000000000000000000000000000000000000000000000000aa11", now.Add(time.Duration(i)*time.Second))
		if err := agreement.Record(h.V, taskObj, obs); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) logText() string { return strings.Join(h.Logs, "\n") }

// ---------------------------------------------------------------------------
// §9.1 Tier equivalence
// ---------------------------------------------------------------------------

// Test01_TierEquivalence is §9.1: identical Factory binary, Micro and Mini
// configs, same lifecycle.
//
// It checks the property two ways, because either alone is easy to satisfy
// dishonestly. First structurally: both templates wire the same type through the
// same function, and that function never reads the profile name. Then
// behaviourally: both run a complete pass over the same repository state and
// differ only where their declared roles differ.
func Test01_TierEquivalence(t *testing.T) {
	micro := profile.Micro("micro-a")
	mini := profile.Mini("mini-a")

	if err := micro.Validate(); err != nil {
		t.Fatalf("the shipped micro template is invalid: %v", err)
	}
	if err := mini.Validate(); err != nil {
		t.Fatalf("the shipped mini template is invalid: %v", err)
	}

	// Micro ships with roles verify and build, and attempting opt-in (§3.1).
	microCaps := micro.Capabilities()
	if microCaps.Has(cell.RoleAttempt) {
		t.Fatal("the micro template ships with the attempt role; attempting must be opt-in (§3.1)")
	}
	if !microCaps.Has(cell.RoleVerify) || !microCaps.Has(cell.RoleBuild) {
		t.Fatalf("the micro template is not a verify/build cell: %v", microCaps.Roles)
	}
	if !mini.Capabilities().Has(cell.RoleAttempt) {
		t.Fatal("the mini template does not attempt")
	}

	// The two configs must differ only in field values. Wiring them produces the
	// same concrete type through the same call.
	for _, cfg := range []profile.Config{micro, mini} {
		built, err := cfg.Wire(varvigcli.NewFake(cfg.CellID))
		if err != nil {
			t.Fatalf("wiring %s: %v", cfg.Profile, err)
		}
		var _ *loop.Cell = built.Cell
		if built.Cell == nil || built.Ledger == nil || built.Switch == nil {
			t.Fatalf("%s wired an incomplete cell", cfg.Profile)
		}
	}

	// Behaviour: the same lifecycle runs for both. The Micro cell verifies a
	// peer's attempt; the Mini cell attempts. Neither takes a different path
	// through the loop to get there.
	ctx := context.Background()
	microH := newHarness(t, opts{
		cellID: "micro-a",
		roles:  []cell.Role{cell.RoleBuild, cell.RoleVerify},
		tier:   cell.TierNone,
		budget: budget.Budget{VerifyConcurrent: 2, StorageGB: 10},
	})
	miniH := newHarness(t, defaultOpts("mini-a"))

	// Give the Micro cell a peer attempt to verify, so both cells have work.
	seedPeerAttempt(t, microH.V, "mini-b", taskID, "c3feed0000000000000000000000000000000000000000000000000000000003")

	microRep, err := microH.Cell.Once(ctx)
	if err != nil {
		t.Fatalf("micro pass failed: %v", err)
	}
	miniRep, err := miniH.Cell.Once(ctx)
	if err != nil {
		t.Fatalf("mini pass failed: %v", err)
	}

	if len(microRep.Verified) != 1 {
		t.Fatalf("micro verified %d attempts, want 1: %+v", len(microRep.Verified), microRep)
	}
	if len(miniRep.Attempts) != 1 {
		t.Fatalf("mini made %d attempts, want 1: %+v", len(miniRep.Attempts), miniRep)
	}
	// Both observed the same repository state and both completed the same ten
	// steps: the difference is which roles they hold, not which code ran.
	if microRep.Observed != miniRep.Observed {
		t.Fatalf("the two tiers observed different state: %d vs %d", microRep.Observed, miniRep.Observed)
	}
	if got := microRep.Skipped[claim.SkipNotAttempting]; got != 1 {
		t.Fatalf("micro skipped attempting %d times, want 1 with the reason recorded", got)
	}
	// Both produced evidence with an environment hash: the verification half of
	// the lifecycle is identical.
	if microRep.Verified[0].Evidence.Environment == "" {
		t.Fatal("micro produced evidence with no environment")
	}
	if miniRep.Attempts[0].Environment == "" {
		t.Fatal("mini produced an attempt with no environment")
	}
}

// seedPeerAttempt writes an attempt authored by another cell, so a verify-only
// cell has something to verify.
func seedPeerAttempt(t *testing.T, v *varvigcli.Fake, peerCell, task, change string) cell.Attempt {
	t.Helper()
	att := cell.Attempt{CellID: peerCell, Task: task, N: 1, Change: change, CreatedAt: now.Unix() - 60}
	payload, err := cell.Canonical(att)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.PutBlob(payload)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := cell.AttemptRef(peerCell, task, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.UpdateRef(ref, id, ""); err != nil {
		t.Fatal(err)
	}
	return att
}

// ---------------------------------------------------------------------------
// §9.2 Partition duplicates
// ---------------------------------------------------------------------------

// Test02_PartitionDuplicates is §9.2: two cells claim the same task while
// partitioned; both attempts survive reconnect.
//
// The spec is explicit that this is *correct behaviour* and the test exists to
// keep it that way. Duplicate attempts are the point — branching is search — so
// the failure this guards against is somebody "fixing" it by adding consensus.
func Test02_PartitionDuplicates(t *testing.T) {
	ctx := context.Background()
	upstream := varvigcli.NewFake("upstream")

	a := newHarness(t, withUpstream(defaultOpts("mini-a"), upstream))
	b := newHarness(t, withUpstream(defaultOpts("mini-b"), upstream))
	// Both cells yield to fresh foreign claims — the politeness default. It must
	// make no difference here, because a partitioned cell cannot see the other's
	// claim. If this test ever passes only with yielding off, claims have become
	// exclusive.
	a.Cell.YieldToFreshClaims = true
	b.Cell.YieldToFreshClaims = true

	a.V.Partitioned = true
	b.V.Partitioned = true

	repA, err := a.Cell.Once(ctx)
	if err != nil {
		t.Fatalf("cell a: %v", err)
	}
	repB, err := b.Cell.Once(ctx)
	if err != nil {
		t.Fatalf("cell b: %v", err)
	}
	if len(repA.Attempts) != 1 || len(repB.Attempts) != 1 {
		t.Fatalf("both partitioned cells should have attempted: a=%d b=%d", len(repA.Attempts), len(repB.Attempts))
	}
	if !repA.Offline || !repB.Offline {
		t.Fatal("a partitioned cell did not report itself offline")
	}

	// Reconnect. Both attempts must reach upstream, and neither may overwrite the
	// other — which is a property of the *ref naming*, not of a merge: each cell
	// writes under its own cell id.
	a.V.Partitioned = false
	b.V.Partitioned = false
	if err := a.V.Push("upstream", ""); err != nil {
		t.Fatalf("cell a push after reconnect: %v", err)
	}
	if err := b.V.Push("upstream", ""); err != nil {
		t.Fatalf("cell b push after reconnect: %v", err)
	}

	refA, err := cell.AttemptRef("mini-a", taskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	refB, err := cell.AttemptRef("mini-b", taskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	upstreamRefs := upstream.RefSnapshot()
	if _, ok := upstreamRefs[refA]; !ok {
		t.Fatalf("cell a's attempt did not survive reconnect; upstream has %v", keys(upstreamRefs))
	}
	if _, ok := upstreamRefs[refB]; !ok {
		t.Fatalf("cell b's attempt did not survive reconnect; upstream has %v", keys(upstreamRefs))
	}
	// Both claims survive too, and they are distinct refs — so neither CAS could
	// have refused the other.
	claimA, _ := cell.ClaimRef("mini-a", taskID)
	claimB, _ := cell.ClaimRef("mini-b", taskID)
	if _, ok := upstreamRefs[claimA]; !ok {
		t.Fatal("cell a's claim did not reach upstream")
	}
	if _, ok := upstreamRefs[claimB]; !ok {
		t.Fatal("cell b's claim did not reach upstream")
	}
	if claimA == claimB {
		t.Fatal("two cells share a claim ref name; claims would then be exclusive")
	}
}

func withUpstream(o opts, up *varvigcli.Fake) opts {
	o.upstream = up
	return o
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// §9.3 Offline attempt
// ---------------------------------------------------------------------------

// Test03_OfflineAttempt is §9.3: a full lifecycle with upstream unreachable
// throughout, then reconcile.
//
// This is the property that makes the cell model worth having (§5.1): local-first
// operation is required, not merely tolerated. Useful state already exists
// locally as immutable objects, so nothing is stranded by upstream being gone.
func Test03_OfflineAttempt(t *testing.T) {
	ctx := context.Background()
	upstream := varvigcli.NewFake("upstream")
	h := newHarness(t, withUpstream(defaultOpts("mini-a"), upstream))
	h.V.Partitioned = true

	rep, err := h.Cell.Once(ctx)
	if err != nil {
		t.Fatalf("a disconnected cell failed its pass: %v", err)
	}
	if !rep.Offline {
		t.Fatal("the cell did not report itself offline")
	}
	if len(rep.Attempts) != 1 {
		t.Fatalf("a disconnected cell made %d attempts, want 1", len(rep.Attempts))
	}
	att := rep.Attempts[0]

	// Every step of the lifecycle completed locally: claim, change, evidence,
	// environment, attempt ref, pin.
	local := h.V.RefSnapshot()
	claimRef, _ := cell.ClaimRef("mini-a", taskID)
	attemptRef, _ := cell.AttemptRef("mini-a", taskID, 1)
	for _, want := range []string{claimRef, attemptRef} {
		if _, ok := local[want]; !ok {
			t.Fatalf("offline pass did not write %s; refs: %v", want, keys(local))
		}
	}
	pinPrefix, _ := cell.PinPeerPrefix("mini-a")
	pinned := false
	for name := range local {
		if strings.HasPrefix(name, pinPrefix) {
			pinned = true
		}
	}
	if !pinned {
		t.Fatal("offline pass recorded no pin, so upstream would not know to retain the attempt")
	}
	evidence, err := h.V.Notes(att.Change, cell.NoteEvidence)
	if err != nil || len(evidence) == 0 {
		t.Fatalf("offline pass wrote no evidence: %v", err)
	}
	if att.Environment == "" {
		t.Fatal("offline pass produced no environment hash")
	}
	// The offline spend was charged against the tighter offline cap (§7).
	if got := h.Ledger.Snapshot(now).OfflineSpent; got == 0 {
		t.Fatal("offline inference was not charged against the offline cap")
	}

	// Reconcile. Nothing was lost and nothing needed a controller to recover it.
	h.V.Partitioned = false
	if err := h.V.Fetch("upstream", ""); err != nil {
		t.Fatalf("fetch after reconnect: %v", err)
	}
	if err := h.V.Push("upstream", ""); err != nil {
		t.Fatalf("push after reconnect: %v", err)
	}
	if _, ok := upstream.RefSnapshot()[attemptRef]; !ok {
		t.Fatal("the offline attempt did not reach upstream after reconnect")
	}
}

// ---------------------------------------------------------------------------
// §9.4 Environment determinism
// ---------------------------------------------------------------------------

// Test04_EnvironmentDeterminism is §9.4: same adapter, two invocations,
// identical environment hash.
//
// The unit-level halves live in cell/ (canonical encoding) and sandbox/ (a real
// probe run twice). This is the federation-level claim the other two exist to
// support: two cells configured identically produce the same hash, so their
// evidence compares as same-class — and two cells configured differently do not.
func Test04_EnvironmentDeterminism(t *testing.T) {
	ctx := context.Background()

	hashFor := func(o opts) string {
		h := newHarness(t, o)
		rep, err := h.Cell.Once(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Attempts) != 1 {
			t.Fatalf("expected one attempt, got %d", len(rep.Attempts))
		}
		return rep.Attempts[0].Environment
	}

	first := hashFor(defaultOpts("mini-a"))
	second := hashFor(defaultOpts("mini-b"))
	if first != second {
		t.Fatalf("two identically configured cells produced different environment hashes:\n %s\n %s", first, second)
	}
	if !strings.HasPrefix(first, cell.HashAlgorithm+":") {
		t.Fatalf("environment hash %q carries no self-describing label", first)
	}

	// A genuinely different ground must hash differently, or the comparison would
	// admit everything.
	differentPlatform := defaultOpts("mini-c")
	differentPlatform.platform = "darwin/arm64"
	if other := hashFor(differentPlatform); other == first {
		t.Fatal("a different platform produced the same environment hash")
	}

	// And two different platforms are cross-class, which must defer rather than
	// compare equal (CELL.md §4.4).
	linux := cell.Environment{Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.24.7"}}
	darwin := cell.Environment{Platform: "darwin/arm64", Toolchains: map[string]string{"go": "1.24.7"}}
	if cell.SameClass(linux, darwin) {
		t.Fatal("cross-platform environments compared as the same class")
	}
}

// ---------------------------------------------------------------------------
// §9.5 Self-verification refusal
// ---------------------------------------------------------------------------

// Test05_SelfVerificationRefusal is §9.5: an attempt whose only evidence is from
// its own cell is never autonomously promoted.
//
// This is the load-bearing condition of the whole autonomous story (§3.2, §6.3).
// A signature proves who asserted a test result, not that the run was honest, so
// independence is the only leverage available — and a cell that could verify its
// own work into production would have spent it.
func Test05_SelfVerificationRefusal(t *testing.T) {
	ctx := context.Background()
	const fp = "SHA256:selfverify"
	o := defaultOpts("mini-a")
	o.switchPath = filepath.Join(t.TempDir(), "promotion.json")
	o.fingerprint = fp
	o.baselines = map[string]cell.Environment{
		"src/": {Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.24.7"}},
	}
	h := newHarness(t, o)
	h.allowGate()
	h.recordAgreement("src/", 25)
	if err := h.Sw.SetMode(promote.ModeAutonomous); err != nil {
		t.Fatal(err)
	}
	if err := h.Sw.EnableAutonomous("src/"); err != nil {
		t.Fatal(err)
	}

	// One pass: the cell attempts and verifies its own attempt. Every other §6.3
	// condition is satisfied, so independence is the only thing standing between
	// this attempt and promotion.
	rep, err := h.Cell.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Promotions) == 0 {
		t.Fatal("no promotion was evaluated, so nothing was refused")
	}
	for _, out := range rep.Promotions {
		if out.Promoted {
			t.Fatalf("a self-verified attempt was autonomously promoted: %s", out.Summary())
		}
		if !hasFailure(out, promote.CondIndependentEvidence) {
			t.Fatalf("the refusal did not name independent evidence: %s", out.Summary())
		}
	}

	// Now give the attempt evidence from a *different* cell and the same
	// promotion succeeds — which is what proves the refusal was about
	// independence and not about something else quietly blocking it.
	att := rep.Attempts[0]
	peerEvidence(t, h.V, "micro-b", att.Task, att.Change, att.Environment)
	req := promote.Request{
		Attempt:      cell.Attempt{CellID: "mini-a", Task: taskID, N: 1, Change: att.Change, Environment: att.Environment, CreatedAt: now.Unix() - 10},
		Scope:        "src/",
		Ticket:       taskID,
		TicketObject: taskObj,
		Ref:          "refs/heads/main",
	}
	req.Evidence, req.Environments = loadEvidence(t, h.V, att.Change)
	req.Baseline = baselinePtr(o.baselines["src/"])
	if err := h.V.SpecScore(taskID, att.Change, 1); err != nil {
		t.Fatal(err)
	}

	out, err := h.Cell.Promoter.Promote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Promoted {
		t.Fatalf("independently verified attempt was still refused: %s", out.Summary())
	}
}

func hasFailure(out promote.Outcome, want promote.Condition) bool {
	for _, f := range out.Failed {
		if f.Condition == want {
			return true
		}
	}
	return false
}

func baselinePtr(e cell.Environment) *cell.Environment { return &e }

// peerEvidence writes passing evidence authored by another cell, in the same
// environment, so the only thing that changed is who asserted it.
func peerEvidence(t *testing.T, v *varvigcli.Fake, peerCell, task, change, envHash string) {
	t.Helper()
	ev := cell.Evidence{
		Attempt: change, Task: task, CellID: peerCell, Environment: envHash,
		Checks:     []cell.Check{{Name: "unit", Status: cell.StatusPass}},
		ProducedAt: now.Unix(),
	}
	payload, err := cell.Canonical(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.AddNote(change, cell.NoteEvidence, payload); err != nil {
		t.Fatal(err)
	}
}

func loadEvidence(t *testing.T, v *varvigcli.Fake, change string) ([]cell.Evidence, map[string]cell.Environment) {
	t.Helper()
	notes, err := v.Notes(change, cell.NoteEvidence)
	if err != nil {
		t.Fatal(err)
	}
	var out []cell.Evidence
	for _, n := range notes {
		var ev cell.Evidence
		if err := jsonUnmarshal(n.Payload, &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	envs := map[string]cell.Environment{}
	envNotes, err := v.Notes(change, cell.NoteEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range envNotes {
		var env cell.Environment
		if err := jsonUnmarshal(n.Payload, &env); err != nil {
			t.Fatal(err)
		}
		hash, err := env.Hash()
		if err != nil {
			t.Fatal(err)
		}
		envs[hash] = env
	}
	return out, envs
}

// ---------------------------------------------------------------------------
// §9.6 Budget halt
// ---------------------------------------------------------------------------

// Test06_BudgetHalt is §9.6: the cell stops claiming at its cap and does not
// silently downgrade models.
//
// The second half is the one worth testing, and the harder one to state. Halting
// is visible; degrading is not — a cell that quietly switched to a smaller model
// would keep producing attempts, and the damage would show up weeks later as a
// selection statistic nobody can explain (§7).
func Test06_BudgetHalt(t *testing.T) {
	ctx := context.Background()
	o := defaultOpts("mini-a")
	// One unit of budget, one unit per call: the second pass must halt.
	o.budget = budget.Budget{InferenceDaily: 1, PerCallCost: 1, VerifyConcurrent: 2, StorageGB: 10, AttemptsDefault: 1}
	h := newHarness(t, o)
	// Allow repeat attempts, so "already attempted" cannot be what stops the
	// second pass. Budget must be the thing that does.
	h.Cell.MaxAttemptsPerCell = 5

	first, err := h.Cell.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Attempts) != 1 {
		t.Fatalf("first pass made %d attempts, want 1", len(first.Attempts))
	}
	callsAfterFirst := h.Model.Calls
	modelAfterFirst := h.Cell.Capabilities.Inference.Models

	second, err := h.Cell.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Attempts) != 0 {
		t.Fatalf("the cell kept attempting past its cap: %+v", second.Attempts)
	}
	if got := second.Skipped[claim.SkipBudget]; got != 1 {
		t.Fatalf("the skip was not attributed to budget: %+v", second.Skipped)
	}
	// It stopped spending, not just stopped reporting.
	if h.Model.Calls != callsAfterFirst {
		t.Fatalf("the model was called %d more times after the cap", h.Model.Calls-callsAfterFirst)
	}
	// And it says so.
	if !strings.Contains(h.logText(), "halted") {
		t.Fatalf("a halted cell did not say so:\n%s", h.logText())
	}

	// No silent downgrade: the cell's advertised model is unchanged, and the
	// runtime it holds is the same object it started with. A cell that swapped
	// either would be producing attempts from a model its capabilities do not
	// name.
	if len(h.Cell.Capabilities.Inference.Models) != len(modelAfterFirst) {
		t.Fatal("the cell changed its advertised models under budget pressure")
	}
	if h.Cell.Capabilities.Inference.Tier != cell.TierLarge {
		t.Fatalf("the cell downgraded its tier to %q under budget pressure", h.Cell.Capabilities.Inference.Tier)
	}
	if h.Cell.Inference != inference.Runtime(h.Model) {
		t.Fatal("the cell swapped its model runtime under budget pressure")
	}

	// The ledger refuses by name, with the numbers.
	d := h.Ledger.CanSpend(now, false)
	if d.OK {
		t.Fatal("the ledger still permits spending past the cap")
	}
	if !strings.Contains(d.String(), "cap") {
		t.Fatalf("the refusal does not name the cap: %s", d)
	}
}

// ---------------------------------------------------------------------------
// §9.7 Kill switch
// ---------------------------------------------------------------------------

// Test07_KillSwitch is §9.7: the mode flip takes effect without a restart, and
// revoking the allowed_keys line stops promotion federation-wide.
//
// Both paths must be tested (§6.5), and they are different mechanisms: the first
// is local and immediate, the second is a repository fact every peer honours.
func Test07_KillSwitch(t *testing.T) {
	ctx := context.Background()
	const fp = "SHA256:killswitch"
	statePath := filepath.Join(t.TempDir(), "promotion.json")

	o := defaultOpts("mini-a")
	o.switchPath = statePath
	o.fingerprint = fp
	o.baselines = map[string]cell.Environment{
		"src/": {Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.24.7"}},
	}
	h := newHarness(t, o)
	h.allowGate()
	h.recordAgreement("src/", 25)
	if err := h.Sw.SetMode(promote.ModeAutonomous); err != nil {
		t.Fatal(err)
	}
	if err := h.Sw.EnableAutonomous("src/"); err != nil {
		t.Fatal(err)
	}

	// Make one attempt with independent evidence, so it is genuinely promotable
	// and the only variable is the switch.
	rep, err := h.Cell.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	att := rep.Attempts[0]
	peerEvidence(t, h.V, "micro-b", att.Task, att.Change, att.Environment)
	if err := h.V.SpecScore(taskID, att.Change, 1); err != nil {
		t.Fatal(err)
	}
	req := promote.Request{
		Attempt:      cell.Attempt{CellID: "mini-a", Task: taskID, N: 1, Change: att.Change, Environment: att.Environment, CreatedAt: now.Unix() - 10},
		Scope:        "src/",
		Ticket:       taskID,
		TicketObject: taskObj,
		Ref:          "refs/heads/main",
		Baseline:     baselinePtr(o.baselines["src/"]),
	}
	req.Evidence, req.Environments = loadEvidence(t, h.V, att.Change)

	out, err := h.Cell.Promoter.Promote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Promoted {
		t.Fatalf("the attempt was not promotable before the kill switch, so the test proves nothing: %s", out.Summary())
	}

	// --- Path 1: the mode flip, no restart ---
	//
	// A *second* Switch over the same file stands in for the CLI process. The
	// running cell keeps its own Switch object; if the flip only took effect on
	// restart, the running cell would still promote.
	cliSwitch, err := promote.NewSwitch(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cliSwitch.SetMode(promote.ModeGated); err != nil {
		t.Fatal(err)
	}
	out, err = h.Cell.Promoter.Promote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Promoted {
		t.Fatal("the running cell kept promoting after the mode was flipped to gated; the kill switch needs a restart")
	}
	if out.Mode != promote.ModeGated {
		t.Fatalf("the running cell still reports mode %q", out.Mode)
	}

	// --- Path 2: allowed_keys revocation ---
	//
	// Back to autonomous, then delete the trust-store line. Revoking the line
	// must be sufficient on its own — the cell's own configuration still says
	// autonomous and still names the enabled path.
	if err := cliSwitch.SetMode(promote.ModeAutonomous); err != nil {
		t.Fatal(err)
	}
	if out, err = h.Cell.Promoter.Promote(ctx, req); err != nil || !out.Promoted {
		t.Fatalf("could not restore autonomous promotion for the second half of the test: %v %s", err, out.Summary())
	}
	h.V.SetTrust() // the line is gone
	out, err = h.Cell.Promoter.Promote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Promoted {
		t.Fatal("promotion continued after the allowed_keys line was revoked")
	}
	if !hasFailure(out, promote.CondTrustGrant) {
		t.Fatalf("the refusal did not name the missing trust grant: %s", out.Summary())
	}
	// The mode is still autonomous: the revocation, not a mode change, is what
	// stopped it.
	if out.Mode != promote.ModeAutonomous {
		t.Fatalf("mode changed to %q; the test no longer isolates the revocation", out.Mode)
	}
}

// ---------------------------------------------------------------------------
// §9.8 Agreement-rate gate
// ---------------------------------------------------------------------------

// Test08_AgreementRateGate is §9.8: autonomous mode refuses to enable below the
// threshold, with a clear message.
//
// "With a clear message" is part of the requirement, not decoration. An operator
// who is refused without a number goes looking for the override.
func Test08_AgreementRateGate(t *testing.T) {
	g := agreement.NewGate(0, 0) // the defaults: 80%, 20 observations

	// Below threshold: refused, and the message carries both numbers.
	low := agreement.Rate{Scope: "src/", Observations: 40, Agreements: 20}
	verdict := g.Allow(low)
	if verdict.Allowed {
		t.Fatalf("a 50%% agreement rate was permitted: %s", verdict)
	}
	for _, want := range []string{"50", "80"} {
		if !strings.Contains(verdict.Reason, want) {
			t.Fatalf("the refusal does not name %s: %s", want, verdict.Reason)
		}
	}

	// At threshold: permitted.
	ok := agreement.Rate{Scope: "src/", Observations: 40, Agreements: 34}
	if v := g.Allow(ok); !v.Allowed {
		t.Fatalf("an 85%% rate was refused: %s", v)
	}

	// No metric at all: refused, and the message says what to do about it. This
	// is §6.3 condition 5 — the metric must *exist* for the scope.
	none := agreement.Rate{Scope: "src/generated/"}
	v := g.Allow(none)
	if v.Allowed {
		t.Fatal("autonomous mode was permitted for a scope with no recorded metric")
	}
	if !strings.Contains(v.Reason, "gated") {
		t.Fatalf("the refusal does not tell the operator to run gated: %s", v.Reason)
	}

	// A tiny sample at 100% must not unlock a scope. A threshold without a
	// minimum sample would let one agreeing promotion license autonomy.
	tiny := agreement.Rate{Scope: "src/", Observations: 1, Agreements: 1}
	if v := g.Allow(tiny); v.Allowed {
		t.Fatalf("a single observation at 100%% unlocked autonomous promotion: %s", v)
	}

	// End to end: a real cell with a poor recorded rate refuses to promote and
	// names the metric.
	ctx := context.Background()
	o := defaultOpts("mini-a")
	o.switchPath = filepath.Join(t.TempDir(), "promotion.json")
	o.fingerprint = "SHA256:agreement"
	o.baselines = map[string]cell.Environment{
		"src/": {Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.24.7"}},
	}
	h := newHarness(t, o)
	h.allowGate()
	if err := h.Sw.SetMode(promote.ModeAutonomous); err != nil {
		t.Fatal(err)
	}
	if err := h.Sw.EnableAutonomous("src/"); err != nil {
		t.Fatal(err)
	}
	// 30 observations, 12 agreements: 40%, well below threshold.
	for i := 0; i < 30; i++ {
		promoted := "aa11top000000000000000000000000000000000000000000000000000000aa11"
		if i >= 12 {
			promoted = "bb22other0000000000000000000000000000000000000000000000000000bb22"
		}
		obs := agreement.Observe("src/", taskID, "aa11top000000000000000000000000000000000000000000000000000000aa11", promoted, now.Add(time.Duration(i)*time.Second))
		if err := agreement.Record(h.V, taskObj, obs); err != nil {
			t.Fatal(err)
		}
	}
	rate, err := agreement.RateFor(h.V, "src/")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Observations != 30 || rate.Agreements != 12 {
		t.Fatalf("recorded rate = %+v, want 12/30", rate)
	}

	rep, err := h.Cell.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	att := rep.Attempts[0]
	peerEvidence(t, h.V, "micro-b", att.Task, att.Change, att.Environment)
	if err := h.V.SpecScore(taskID, att.Change, 1); err != nil {
		t.Fatal(err)
	}
	req := promote.Request{
		Attempt:      cell.Attempt{CellID: "mini-a", Task: taskID, N: 1, Change: att.Change, Environment: att.Environment, CreatedAt: now.Unix() - 10},
		Scope:        "src/",
		Ticket:       taskID,
		TicketObject: taskObj,
		Ref:          "refs/heads/main",
		Baseline:     baselinePtr(o.baselines["src/"]),
	}
	req.Evidence, req.Environments = loadEvidence(t, h.V, att.Change)

	out, err := h.Cell.Promoter.Promote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Promoted {
		t.Fatal("an attempt was autonomously promoted in a scope whose agreement rate is 40%")
	}
	if !hasFailure(out, promote.CondAgreementMetric) {
		t.Fatalf("the refusal did not name the agreement metric: %s", out.Summary())
	}
}

// ---------------------------------------------------------------------------
// §9.9 No second scheduler
// ---------------------------------------------------------------------------

// Test09_NoSecondScheduler is §9.9: assert Factory never assigns read/write sets
// itself; it submits and lets varvig serialize.
//
// The static half is in ./guard, which fails the build on a declaration that
// computes scheduling. This is the behavioural half: the scope a cell hands to
// varvig is the one the ticket declared, and nothing about ordering ever crosses
// the boundary.
func Test09_NoSecondScheduler(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultOpts("mini-a"))

	if _, err := h.Cell.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// The task was submitted with the ticket's own declared read set.
	if !h.V.Called("TaskStart src") {
		t.Fatalf("the cell did not submit the ticket's declared scope to varvig; calls: %v", h.V.Calls)
	}
	// And it went through varvig's speculation pool rather than a Factory-side
	// queue: the pool, the scoring and the selection are varvig's.
	if !h.V.Called("SpecAdd") {
		t.Fatalf("the attempt was not offered to varvig's speculation pool; calls: %v", h.V.Calls)
	}

	// A ticket with no declared scope is skipped as unschedulable rather than
	// having one computed for it. This is the moment a second scheduler would be
	// most tempting: the cell can see the files, so it *could* derive a write
	// set. It must not.
	h2 := newHarness(t, defaultOpts("mini-b"))
	h2.V.AddTicket("d4bad00000000000000000000000000000000000000000000000000000000004", "Do something.", varvigcli.Scope{}, "approved")
	rep, err := h2.Cell.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Skipped[claim.SkipUnschedulable]; got != 1 {
		t.Fatalf("an unscoped ticket was not skipped as unschedulable: %+v", rep.Skipped)
	}

	// Blocking is read from varvig, never derived. A blocked ticket is skipped
	// with varvig's own answer as the reason.
	h3 := newHarness(t, defaultOpts("mini-c"))
	h3.V.SetBlockers(taskID, "e5b10cked000000000000000000000000000000000000000000000000000005")
	rep3, err := h3.Cell.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := rep3.Skipped[claim.SkipBlocked]; got != 1 {
		t.Fatalf("a blocked ticket was not skipped: %+v", rep3.Skipped)
	}
	if len(rep3.Attempts) != 0 {
		t.Fatal("the cell attempted a ticket varvig reported as blocked")
	}
}

// jsonUnmarshal is encoding/json's Unmarshal, wrapped so the helpers above read
// without an import that only they use.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
