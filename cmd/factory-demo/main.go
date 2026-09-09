// Command factory-demo runs the Medium prototype from FACTORY.md §10.7 — two
// cells and one rendezvous peer — end to end, against in-memory fakes.
//
// It exists for the same reason varvig-connectors ships a reference connector:
// the interesting parts of this system are the interactions, and a description
// of an interaction is not a demonstration of one. In four phases it shows:
//
//  1. a Mini cell attempts a ticket, and a Micro cell independently verifies it
//     — which is what makes autonomous promotion defensible at all (§3.2)
//  2. gated promotion evaluates every §6.3 condition, runs the policy module,
//     logs the verdict, and does not act (§10.9)
//  3. a partition: both cells claim and attempt the same task, and both attempts
//     survive reconnect (§9.2) — correct behaviour, not a bug
//  4. autonomous promotion, once the agreement metric exists and the path is
//     enabled — then the kill switch, which stops it without a restart (§6.5)
//
// No varvig binary, no GPU, no network. What it does not demonstrate is
// anything about a real model's output quality; that is the one thing a fake
// cannot stand in for, and the point here is the lifecycle.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/artifact"
	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/effect"
	"github.com/varvig/varvig-factory/gate"
	"github.com/varvig/varvig-factory/inference"
	"github.com/varvig/varvig-factory/loop"
	"github.com/varvig/varvig-factory/promote"
	"github.com/varvig/varvig-factory/sandbox"
	"github.com/varvig/varvig-factory/varvigcli"
)

var (
	clock   = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ticket  = "a1c0ffee00000000000000000000000000000000000000000000000000000001"
	ticketO = "b2dec0de00000000000000000000000000000000000000000000000000000002"
	scope   = varvigcli.Scope{Reads: []string{"src"}, Writes: []string{"src"}}
	branch  = "refs/heads/main"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "factory-demo: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	work, err := os.MkdirTemp("", "factory-demo-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// The rendezvous is a varvig peer and nothing more: no Factory process, no
	// queue, no RPCs (§1.1). Coordination happens by exchanging repository state.
	upstream := varvigcli.NewFake("upstream")
	seedTicket(upstream)

	mini := newCell(work, "mini-a", upstream, attemptRoles(), largeTier())
	micro := newCell(work, "micro-b", upstream, verifyRoles(), noTier())

	// Both cells start from the rendezvous peer's state.
	for _, c := range []*demoCell{mini, micro} {
		if err := c.v.Fetch("upstream", branch); err != nil {
			return err
		}
		if err := c.cell.PublishCapabilities(); err != nil {
			return err
		}
		fmt.Printf("cell %s\n", c.cell.Capabilities)
	}

	// ---- Phase 1: attempt, then independent verification ----
	section("phase 1: mini-a attempts, micro-b verifies independently")

	miniRep, err := mini.cell.Once(ctx)
	if err != nil {
		return err
	}
	fmt.Println(miniRep.Summary())
	if len(miniRep.Attempts) == 0 {
		return fmt.Errorf("mini-a made no attempt")
	}
	att := miniRep.Attempts[0]
	fmt.Printf("  attempt   %s (env %s)\n", short(att.Change), short(att.Environment))
	fmt.Printf("  evidence  %s from mini-a — self-produced, and therefore never enough on its own (§6.3.1)\n",
		verdict(att.Evidence))

	// micro-b picks up the attempt from the rendezvous peer and verifies it. Nothing told it
	// to: it read the repository.
	if err := micro.v.Fetch("upstream", branch); err != nil {
		return err
	}
	microRep, err := micro.cell.Once(ctx)
	if err != nil {
		return err
	}
	fmt.Println(microRep.Summary())
	for _, v := range microRep.Verified {
		fmt.Printf("  evidence  %s from micro-b — a different cell, which is the whole point\n", verdict(v.Evidence))
	}
	if err := micro.v.Push("upstream", branch); err != nil {
		return err
	}
	if err := mini.v.Fetch("upstream", branch); err != nil {
		return err
	}

	// ---- Phase 2: gated promotion runs the gate and does not act ----
	section("phase 2: gated promotion — the policy module runs and logs, without acting (§10.9)")

	// A policy module that would allow this promotion. In a real cell it is a
	// content-addressed wasm object in the repository, run in varvig's WASI
	// sandbox; here it is a function, because the demo has no wasm toolchain.
	for _, c := range []*demoCell{mini, micro} {
		c.v.BindHook(gate.Event, func([]byte) varvigcli.HookResult {
			return varvigcli.HookResult{ExitCode: 0, Stdout: "evidence passes, class matches"}
		})
	}
	req := mini.request(att)
	out, err := mini.cell.Promoter.Promote(ctx, req)
	if err != nil {
		return err
	}
	fmt.Println(indent(out.Summary()))
	fmt.Println("  the cell evaluated every condition and promoted nothing; a higher principal decides")

	// A human promotes, and the cell records whether scoring agreed. That
	// observation is the only honest basis for enabling autonomy later (§6.4).
	if err := mini.v.SpecScore(ticket, att.Change, 1.0); err != nil {
		return err
	}
	promoted, err := mini.v.SpecPromote(ticket, branch)
	if err != nil {
		return err
	}
	obs, err := mini.cell.Promoter.ObservePromotion(req)
	if err != nil {
		return err
	}
	fmt.Printf("  human promoted %s; scoring agreed: %t\n", short(promoted), obs.Agreed)

	// ---- Phase 3: partition ----
	section("phase 3: partition — both cells claim the same task, both attempts survive (§9.2)")

	second := "c3feed0000000000000000000000000000000000000000000000000000000003"
	for _, c := range []*demoCell{mini, micro} {
		c.v.AddTicket(second, "Add B to src.\n", scope, "approved")
	}
	// micro-b takes the attempt role for this phase, so there are two attempting
	// cells to partition. Same binary, same code — one field of configuration.
	micro.cell.Capabilities.Roles = append(micro.cell.Capabilities.Roles, cell.RoleAttempt)
	micro.cell.Capabilities.Inference = largeTier()
	micro.cell.Inference = &inference.Fake{Reply: "--- src/b.go\npackage src\n\nfunc BFromMicro() {}\n", Model: "demo-model"}
	micro.ledgerRefill()

	mini.v.Partitioned = true
	micro.v.Partitioned = true
	fmt.Println("  upstream unreachable from both cells")

	for _, c := range []*demoCell{mini, micro} {
		rep, err := c.cell.Once(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("  %s: %s\n", c.cell.Capabilities.CellID, rep.Summary())
	}

	mini.v.Partitioned = false
	micro.v.Partitioned = false
	if err := mini.v.Push("upstream", branch); err != nil {
		return err
	}
	if err := micro.v.Push("upstream", branch); err != nil {
		return err
	}
	fmt.Println("  reconnected; upstream now holds:")
	refs := upstream.RefSnapshot()
	for _, cellID := range []string{"mini-a", "micro-b"} {
		ref, err := cell.AttemptRef(cellID, second, 1)
		if err != nil {
			return err
		}
		if _, ok := refs[ref]; ok {
			fmt.Printf("    %s\n", ref)
		} else {
			return fmt.Errorf("attempt by %s did not survive reconnect", cellID)
		}
	}
	fmt.Println("  neither overwrote the other: attempt refs are per-cell and immutable (CELL.md §2)")

	// ---- Phase 4: autonomous promotion, then the kill switch ----
	section("phase 4: autonomous promotion, earned per scope — then the kill switch (§6.3, §6.5)")

	// Autonomous mode refuses until the agreement metric exists for the scope.
	if err := mini.sw.SetMode(promote.ModeAutonomous); err != nil {
		return err
	}
	if err := mini.sw.EnableAutonomous("src/"); err != nil {
		return err
	}
	rate, err := agreement.RateFor(mini.project, "src/")
	if err != nil {
		return err
	}
	fmt.Printf("  agreement for src/: %s\n", rate)
	fmt.Printf("  %s\n", agreement.NewGate(0, 0).Allow(rate))

	// Record enough gated promotions for the scope to qualify. In a real
	// deployment this is weeks of ordinary reviewed work, which is the point.
	for i := 0; i < agreement.DefaultMinObservations; i++ {
		o := agreement.Observe("src/", ticket, "top", "top", clock.Add(time.Duration(i)*time.Second))
		if err := agreement.Record(mini.project, ticketO, o); err != nil {
			return err
		}
	}
	rate, err = agreement.RateFor(mini.project, "src/")
	if err != nil {
		return err
	}
	fmt.Printf("  after %d gated promotions: %s — %s\n", agreement.DefaultMinObservations, rate,
		agreement.NewGate(0, 0).Allow(rate))

	// A fresh attempt with independent evidence, and the trust store granting
	// promote at this path and nothing wider.
	mini.v.SetTrust(varvigcli.TrustEntry{
		Fingerprint: mini.fingerprint, Name: "factory-prod", Scope: "src/", Rights: []string{"promote"},
	})
	third, err := mini.attemptFresh(ctx)
	if err != nil {
		return err
	}
	if err := mini.v.SpecScore(third.Task, third.Change, 1.0); err != nil {
		return err
	}
	peerEvidence(mini.v, "micro-b", third)
	freshReq := mini.request(third)

	out, err = mini.cell.Promoter.Promote(ctx, freshReq)
	if err != nil {
		return err
	}
	fmt.Println(indent(out.Summary()))

	// The kill switch. A second Switch over the same file stands in for the CLI:
	// `varvig-factory promote --mode gated`. The running cell keeps its own
	// Switch object and must still stop.
	cli, err := promote.NewSwitch(mini.switchPath)
	if err != nil {
		return err
	}
	if err := cli.SetMode(promote.ModeGated); err != nil {
		return err
	}
	fmt.Println("  --- varvig-factory promote --mode gated ---")
	out, err = mini.cell.Promoter.Promote(ctx, freshReq)
	if err != nil {
		return err
	}
	fmt.Println(indent(out.Summary()))
	fmt.Println("  the running cell stopped promoting with no restart")

	// The other kill switch: revoking the allowed_keys line. It must be
	// sufficient on its own, federation-wide.
	if err := cli.SetMode(promote.ModeAutonomous); err != nil {
		return err
	}
	mini.v.SetTrust()
	fmt.Println("  --- allowed_keys line deleted ---")
	out, err = mini.cell.Promoter.Promote(ctx, freshReq)
	if err != nil {
		return err
	}
	fmt.Println(indent(out.Summary()))
	fmt.Println("  still in autonomous mode, and promoting nothing: the revocation alone stopped it")

	section("phase 5: authority — a lease is spendable offline; a promotion is not (§4.3b, §6.7)")

	// Back to a promotable state: the trust line restored, autonomous mode on.
	mini.v.SetTrust(varvigcli.TrustEntry{
		Fingerprint: mini.fingerprint, Name: "factory-prod", Scope: "src/", Rights: []string{"promote"},
	})

	// The partition. Same request, same rights, same gate — the only thing that
	// changed is that the cell can no longer confirm its trust state is current.
	// Promotion moves a ref that everyone else builds on, and a shared ceiling
	// cannot be enforced from a stale view, so it refuses.
	disconnected := freshReq
	disconnected.Sync = authority.Sync{Configured: true, Reachable: false, At: clock.Add(-72 * time.Hour)}
	fmt.Println("  --- upstream unreachable for three days ---")
	out, err = mini.cell.Promoter.Promote(ctx, disconnected)
	if err != nil {
		return err
	}
	fmt.Println(indent(out.Summary()))

	// Meanwhile the cell keeps proposing. Append-only bounds the damage: a
	// principal revoked in the partition wastes compute and nothing more.
	must(authority.Permit(authority.ActPropose, disconnected.Sync, clock, 0, nil))
	fmt.Println("  proposing is still permitted while disconnected")

	// And it keeps spending what was *exclusively* allocated to it. The overseer
	// committed this 1000 EUR to mini-a when the lease was issued, so no other
	// cell can spend it and no connectivity is needed to know that.
	envelope := authority.Envelope{
		Overseer: "overseer-a", SetAt: clock.Unix(),
		Ceilings: []authority.Ceiling{{
			Capability: "pcb-fabrication@1", Spend: 500000, Unit: "EUR", Quantity: 100, RatePerDay: 4,
		}},
	}
	must(envelope.Validate())
	lease := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20" + short(third.Change), Amount: 100000, Unit: "EUR", Quantity: 20,
		IssuedAt: clock.Unix(), ReclaimAfter: clock.Add(24 * time.Hour).Unix(),
	}
	must(authority.CheckExclusive(envelope, []authority.Lease{lease}))

	// Both live in the repository, under their own ref prefixes, signed and
	// CAS-updated like anything else. Nothing about this asks varvig to change.
	must1(authority.PublishEnvelope(mini.factory, envelope, ""))
	must1(authority.PublishLease(mini.factory, lease, ""))
	fmt.Printf("  %s -> %s\n", must1(cell.EnvelopeRef(envelope.Overseer)),
		short(must1(mini.v.ResolveRef(must1(cell.EnvelopeRef(envelope.Overseer))))))
	issued, _ := must2(authority.LoadLease(mini.factory, "mini-a", "pcb-fabrication@1"))
	fmt.Printf("  %s\n", issued)

	boards := effect.Capability{
		ID:        "pcb-fabrication@1",
		Interface: ifaceHash(map[string]any{"gerber": "string", "quantity": "integer"}),
		Effectful: true,
		CostModel: effect.CostFixed,
	}
	order := effect.Request{
		Capability: boards, Task: ticket, Attempts: 1,
		Payload: map[string]any{"gerber": short(third.Change), "quantity": 5},
		Amount:  32000, Quantity: 5, Unit: "EUR",
		AuthorizedBy: "overseer-a",
	}
	grant := authority.Grant{Envelope: envelope, Lease: &lease}
	dec := effect.Check(order, "mini-a", grant, disconnected.Sync, func() time.Time { return clock }, 0)
	fmt.Printf("  offline order of 320 EUR inside a 1000 EUR lease: allowed=%v key=%s…\n", dec.Allowed, dec.Key[:12])

	// The same intent, retried after the response was lost. Note the payload is
	// handed back with its keys in the other order: the key is a function of what
	// the action *is*, so this is one order, not two.
	retry := order
	retry.Payload = map[string]any{"quantity": 5, "gerber": short(third.Change)}
	again := effect.Check(retry, "mini-a", grant, disconnected.Sync, func() time.Time { return clock }, 0)
	fmt.Printf("  the retry after a lost response derives the same key: %v\n", again.Key == dec.Key)

	// Reserve, execute, settle. The key is claimed in a ref before the effect is
	// attempted, create-only — and the same write holds the lease headroom, so a
	// second pending order cannot pass the same headroom check.
	current, readAt := must2(authority.LoadLease(mini.factory, "mini-a", "pcb-fabrication@1"))
	held := authority.Grant{Envelope: envelope, Lease: &current, LeaseHash: readAt}
	claim := must1(effect.Reserve(mini.factory, mini.project, order, "mini-a", held, clock.Unix(), 3600))
	fmt.Printf("  reserved: %s\n", claim.Reservation)
	fmt.Printf("  the lease now holds %s EUR against it, leaving %s of %s\n",
		claim.Lease.Reserved, claim.Lease.Headroom(), claim.Lease.Amount)

	// A second order that fits the allocation but not the remaining headroom is
	// refused while the first is still outstanding. Counting only settled spend
	// would wave it through and the two together would exceed the lease.
	competing := order
	competing.Task, competing.Amount = ticket+"-b", 80000
	_, tooMuch := effect.Reserve(mini.factory, mini.project, competing, "mini-a",
		authority.Grant{Envelope: envelope, Lease: &claim.Lease, LeaseHash: claim.LeaseHash}, clock.Unix()+1, 3600)
	fmt.Printf("  a second 800 EUR order while the first is pending: %v\n", tooMuch != nil)

	// The order goes through, and settlement converts the hold into spend at the
	// price actually charged rather than the one quoted.
	claim = must1(effect.Settle(mini.factory, mini.project, claim, "PO-90210", 35540, clock.Add(time.Minute).Unix()))
	fmt.Printf("  settled: %s EUR spent of %s, %s left (%s)\n",
		claim.Lease.Spent, claim.Lease.Amount, claim.Lease.Headroom(), claim.Reservation.Detail)

	// The same action again is refused by varvig's ordinary ref CAS, and told
	// what happened rather than placing a second order.
	_, repeat := effect.Reserve(mini.factory, mini.project, retry, "mini-a",
		authority.Grant{Envelope: envelope, Lease: &claim.Lease, LeaseHash: claim.LeaseHash}, clock.Add(time.Hour).Unix(), 3600)
	fmt.Printf("  the same action reserved again: %v\n", repeat)

	// Exposure is what the overseer reasons about, and it is the sum of lease
	// *headroom*: what is spent is gone, and what is held may already be an order
	// at the far end. Neither is still allocatable.
	fmt.Printf("  outstanding exposure for pcb-fabrication@1: %s EUR\n",
		authority.Exposure(must1(authority.Leases(mini.factory, "")))["pcb-fabrication@1"])
	lease = claim.Lease

	// The overseer tightens the envelope to 400 EUR — below what this lease still
	// has. The cell honours it before its next action, with no sync and no
	// reissued lease, because adopting a tighter ceiling can only reduce spend.
	tightened := envelope
	tightened.Ceilings = []authority.Ceiling{{
		Capability: "pcb-fabrication@1", Spend: 40000, Unit: "EUR", Quantity: 100, RatePerDay: 4,
	}}
	must1(authority.PublishEnvelope(mini.factory, tightened,
		must1(mini.v.ResolveRef(must1(cell.EnvelopeRef(envelope.Overseer))))))
	bounded := must1(authority.Grant{Envelope: tightened, Lease: &claim.Lease}.Bounded())
	fmt.Printf("  --- overseer tightens the envelope to 400 EUR ---\n")
	fmt.Printf("  the lease still reads %s EUR; spendable is now %s\n", claim.Lease.Amount, bounded.Headroom())

	// Loosening, by contrast, grants nothing: the minimum is still the lease, so
	// more headroom needs a new lease the overseer has to write.
	loosened := envelope
	loosened.Ceilings = []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 9999900, Unit: "EUR"}}
	wide := must1(authority.Grant{Envelope: loosened, Lease: &claim.Lease}.Bounded())
	fmt.Printf("  a loosened envelope leaves it at %s: widening needs a new lease\n", wide.Headroom())
	envelope = tightened

	// Beyond the lease escalates rather than drawing on the 5000 EUR envelope.
	// A lease that can be exceeded is advisory, and an advisory exclusive
	// allocation is a shared one.
	tooBig := order
	tooBig.Amount, tooBig.Quantity = 90000, 12
	lease = claim.Lease
	beyond := effect.Check(tooBig, "mini-a", authority.Grant{Envelope: envelope, Lease: &lease},
		disconnected.Sync, func() time.Time { return clock }, 0)
	fmt.Printf("  a 900 EUR order with %s EUR spendable: allowed=%v escalate=%v\n", bounded.Headroom(), beyond.Allowed, beyond.Escalate)
	fmt.Println(indent(beyond.Error()))

	// And the cell cannot authorize its own order, holding a promote key or not.
	// Promote rights move refs; they are not a licence to spend money.
	itself := order
	itself.AuthorizedBy = "mini-a"
	// Checked against the pre-tightening envelope so this beat shows one rule
	// failing, not two: the point here is the principal, not the ceiling.
	self := effect.Check(itself, "mini-a", authority.Grant{Envelope: loosened, Lease: &lease},
		disconnected.Sync, func() time.Time { return clock }, 0)
	fmt.Printf("  mini-a authorizing its own order, promote key in hand: allowed=%v escalate=%v\n", self.Allowed, self.Escalate)
	fmt.Println(indent(self.Error()))

	section("phase 6: a ticket orders a thing — the loop's effectful branch (§6.7, §7.1)")

	// Until this existed, everything above was a library with no path from a
	// ticket to an order. A ticket names an effectful capability and its
	// parameters, and is thereby off the speculation path entirely.
	boardTicket := "c3feed" + strings.Repeat("0", 57) + "7"
	boardSpec := "Order the prototype run.\n" +
		"factory-requires: effect=pcb-fabrication@1 interface=" + boards.Interface + "\n" +
		`factory-effect: {"gerber":"rev-c","quantity":2}`
	// Seeded into micro-b's own repository: each cell here has its own, syncing
	// through a shared upstream, which is what makes the partition in phase 3
	// real rather than simulated.
	micro.v.AddTicket(boardTicket, boardSpec, varvigcli.Scope{Reads: []string{"hardware"}, Writes: []string{"hardware"}}, "approved")
	must1(authority.PublishEnvelope(micro.factory, envelope, ""))

	// A fresh lease, since the one above is nearly spent, and a fake executor
	// standing in for the fab. The fake counts how many times the effect really
	// happened, which is the number every guard in this system is about.
	must1(authority.PublishLease(micro.factory, authority.Lease{
		CellID: "micro-b", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: 50000, Unit: "EUR", Quantity: 10, IssuedAt: clock.Unix(),
	}, ""))
	fab := effect.NewFake(boards, 18000, "EUR")

	// Note which cell this is: micro-b, the CPU-local verify/build cell with no
	// model at all. Authority to spend is a lease, not a GPU.
	micro.cell.Capabilities.Effects = []cell.EffectCapability{{ID: boards.ID, Interface: boards.Interface}}
	micro.cell.Executors = effect.Executors{fab}
	micro.cell.EffectAuthorizedBy = "overseer-a"
	micro.cell.EffectTTL = 3600

	// Driven through the real loop, not a private entry point: the ticket goes
	// through claim policy and everything else a pass does.
	pass := must1(micro.cell.Once(ctx))
	for _, e := range pass.Effects {
		fmt.Printf("  %s\n", e)
	}
	fmt.Printf("  the effect happened %d time(s)\n", fab.Count())

	// A second pass — a restart, a re-observed ticket, a rerun. Nothing orders
	// again: the cell has already acted, so it does not even re-claim.
	secondPass := must1(micro.cell.Once(ctx))
	fmt.Printf("  a second pass produced %d effect(s); the fab was called %d time(s) in total\n",
		len(secondPass.Effects), fab.Count())

	settledLease, _ := must2(authority.LoadLease(micro.factory, "micro-b", "pcb-fabrication@1"))
	fmt.Printf("  micro-b's lease: %s of %s EUR spent — and micro-b holds no model at all\n",
		settledLease.Spent, settledLease.Amount)

	section("done")
	fmt.Println("Every step above was a write to repository state. No cell ever told another")
	fmt.Println("cell what to do, and no process held a queue.")
	return nil
}

// demoCell is a cell plus the handles the demo needs to poke at it.
type demoCell struct {
	cell *loop.Cell
	v    *varvigcli.Fake
	// The demo is a single-project factory, so one replica serves both roles.
	// The handles are still two, because the call sites still say which role
	// they are in — which is what keeps the demo an illustration of the real
	// shape rather than of the shortcut.
	factory     varvigcli.FactoryRepo
	project     varvigcli.ProjectRepo
	sw          *promote.Switch
	ledger      *budget.Ledger
	switchPath  string
	fingerprint string
	budget      budget.Budget
	work        string
}

func newCell(work, id string, upstream *varvigcli.Fake, roles []cell.Role, inf cell.Inference) *demoCell {
	v := varvigcli.NewFake(id)
	seedTicket(v)
	v.Upstream = upstream

	b := budget.Budget{InferenceDaily: 10000, PerCallCost: 100, VerifyConcurrent: 2, StorageGB: 10, AttemptsDefault: 1}
	if inf.Tier == cell.TierNone {
		// A verify/build cell holds no inference budget: it could only spend one
		// by being misconfigured (CELL.md §8).
		b = budget.Budget{VerifyConcurrent: 2, StorageGB: 10}
	}
	ledger, err := budget.NewLedger(b, "", clock)
	must(err)

	switchPath := filepath.Join(work, id, "promotion.json")
	sw, err := promote.NewSwitch(switchPath)
	must(err)

	factory, project := varvigcli.Collapsed(v)
	d := &demoCell{v: v, factory: factory, project: project,
		sw: sw, ledger: ledger, switchPath: switchPath,
		fingerprint: "SHA256:" + id, budget: b, work: work}

	var runtime inference.Runtime = inference.None{}
	if inf.Tier != cell.TierNone {
		runtime = &inference.Fake{
			Reply: "--- src/a.go\npackage src\n\nfunc AFrom" + id + "() {}\n",
			Model: "demo-model",
		}
	}
	d.cell = &loop.Cell{
		Capabilities: cell.Capabilities{
			CellID: id, Inference: inf,
			Build: []string{"go"}, Test: []string{"unit"}, Roles: roles,
		},
		Factory:   factory,
		Project:   project,
		Inference: runtime,
		Sandbox:   &sandbox.Fake{},
		Artifacts: &artifact.LocalCAS{Root: filepath.Join(work, id, "artifacts")},
		Ledger:    ledger,
		// The demo's one replica serves both roles, so the same rendezvous set
		// serves both — stated explicitly, because there is no fallback that
		// would guess it (see profile.Config.FactoryRendezvous).
		Rendezvous:        loop.Peers{"upstream"},
		FactoryRendezvous: loop.Peers{"upstream"},
		Branch:            branch,
		Checks: []loop.Check{
			{Name: "build", Command: []string{"true"}, Kind: cell.RoleBuild},
			{Name: "unit", Command: []string{"true"}, Kind: cell.RoleVerify},
		},
		ClaimTTL: 30 * time.Minute,
		TaskTTL:  time.Hour,
		WorkDir:  filepath.Join(work, id, "checkouts"),
		Baselines: map[string]cell.Environment{
			"src/": {Platform: "linux/amd64", Toolchains: map[string]string{"go": "1.24.7"}},
		},
		Now: func() time.Time { return clock },
	}
	d.cell.Promoter = &promote.Promoter{
		Project: project, Switch: sw, Gate: gate.Module{Project: project},
		Agreement:   agreement.NewGate(0, 0),
		Reverify:    d.cell,
		CellID:      id,
		Fingerprint: d.fingerprint,
		Now:         func() time.Time { return clock },
	}
	return d
}

// ledgerRefill gives a cell that has just taken the attempt role a budget to
// attempt with — the demo's stand-in for an operator editing the config.
func (d *demoCell) ledgerRefill() {
	b := budget.Budget{InferenceDaily: 10000, PerCallCost: 100, VerifyConcurrent: 2, StorageGB: 10, AttemptsDefault: 1}
	ledger, err := budget.NewLedger(b, "", clock)
	must(err)
	d.ledger, d.cell.Ledger = ledger, ledger
}

// attemptFresh makes one more attempt at a new ticket, so a promotion decision
// has something not already promoted to consider.
func (d *demoCell) attemptFresh(ctx context.Context) (loop.AttemptResult, error) {
	fresh := "d4face0000000000000000000000000000000000000000000000000000000004"
	d.v.AddTicket(fresh, "Add C to src.\n", scope, "approved")
	rep, err := d.cell.Once(ctx)
	if err != nil {
		return loop.AttemptResult{}, err
	}
	for _, a := range rep.Attempts {
		if a.Task == fresh {
			return a, nil
		}
	}
	return loop.AttemptResult{}, fmt.Errorf("no attempt at %s", short(fresh))
}

// request assembles a promotion decision's inputs by reading them back out of the
// repository, exactly as the loop does.
func (d *demoCell) request(att loop.AttemptResult) promote.Request {
	req := promote.Request{
		Attempt: cell.Attempt{
			CellID: d.cell.Capabilities.CellID, Task: att.Task, N: att.N,
			Change: att.Change, Environment: att.Environment, CreatedAt: clock.Unix() - 60,
		},
		Environments: map[string]cell.Environment{},
		Scope:        "src/",
		Ticket:       att.Task,
		TicketObject: ticketO,
		Ref:          branch,
	}
	baseline := d.cell.Baselines["src/"]
	req.Baseline = &baseline

	notes, err := d.v.Notes(att.Change, cell.NoteEvidence)
	must(err)
	for _, n := range notes {
		var ev cell.Evidence
		if err := jsonUnmarshal(n.Payload, &ev); err == nil {
			req.Evidence = append(req.Evidence, ev)
		}
	}
	envNotes, err := d.v.Notes(att.Change, cell.NoteEnvironment)
	must(err)
	for _, n := range envNotes {
		var env cell.Environment
		if err := jsonUnmarshal(n.Payload, &env); err == nil {
			if h, herr := env.Hash(); herr == nil {
				req.Environments[h] = env
			}
		}
	}
	return req
}

// must2 is must for a call returning two values and an error.
func must2[A, B any](a A, b B, err error) (A, B) {
	must(err)
	return a, b
}

// must1 is must for a call that also returns a value.
func must1[T any](v T, err error) T {
	must(err)
	return v
}

// ifaceHash stands in for a capability registry: hash the interface schema and
// encode it as an object hash. A capability reference binds to this, not to the
// alias, so two factories using the same alias for different interfaces cannot
// be mistaken for each other.
func ifaceHash(schema any) string {
	labelled, err := cell.CanonicalHash(schema)
	must(err)
	mh, err := cell.ToMultihash(labelled)
	must(err)
	return mh
}

func seedTicket(v *varvigcli.Fake) {
	v.AddTicket(ticket, "Add A to src.\n", scope, "approved")
	cur, err := v.ResolveRef("refs/varvig/tickets/" + ticket)
	must(err)
	must(v.UpdateRef("refs/varvig/tickets/"+ticket, ticketO, cur))
}

// peerEvidence writes passing evidence from another cell in the same
// environment, which is what §6.3 condition 1 requires and condition 2 compares.
func peerEvidence(v *varvigcli.Fake, peer string, att loop.AttemptResult) {
	ev := cell.Evidence{
		Attempt: att.Change, Task: att.Task, CellID: peer, Environment: att.Environment,
		Checks:     []cell.Check{{Name: "unit", Status: cell.StatusPass}},
		ProducedAt: clock.Unix(),
	}
	payload, err := cell.Canonical(ev)
	must(err)
	must(v.AddNote(att.Change, cell.NoteEvidence, payload))
}

func attemptRoles() []cell.Role {
	return []cell.Role{cell.RoleAttempt, cell.RoleBuild, cell.RoleVerify}
}

// verifyRoles is the Micro default: verify and build, attempting opt-in (§3.1).
func verifyRoles() []cell.Role { return []cell.Role{cell.RoleBuild, cell.RoleVerify} }

func largeTier() cell.Inference {
	return cell.Inference{Tier: cell.TierLarge, Models: []cell.Model{{ID: "demo-model"}}}
}

func noTier() cell.Inference { return cell.Inference{Tier: cell.TierNone} }

func verdict(ev cell.Evidence) string {
	if ev.Passed() {
		return "pass"
	}
	return "not-pass"
}

func section(title string) {
	fmt.Printf("\n=== %s ===\n", title)
}

func indent(s string) string {
	out := ""
	for i, line := range splitLines(s) {
		if i > 0 {
			out += "\n"
		}
		out += "  " + line
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// jsonUnmarshal is encoding/json's Unmarshal, wrapped so the one place that
// needs it does not put a bare import at the top of a demo.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
