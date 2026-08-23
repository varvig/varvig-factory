// Command factory-demo runs the Medium prototype from FACTORY.md §10.7 — two
// cells and one upstream — end to end, against in-memory fakes.
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
	"time"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/artifact"
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
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

	// The upstream is a varvig peer and nothing more: no Factory process, no
	// queue, no RPCs (§1.1). Coordination happens by exchanging repository state.
	upstream := varvigcli.NewFake("upstream")
	seedTicket(upstream)

	mini := newCell(work, "mini-a", upstream, attemptRoles(), largeTier())
	micro := newCell(work, "micro-b", upstream, verifyRoles(), noTier())

	// Both cells start from upstream's state.
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

	// micro-b picks up the attempt from upstream and verifies it. Nothing told it
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
	fmt.Println("  the cell evaluated every condition and promoted nothing; a human decides")

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
	rate, err := agreement.RateFor(mini.v, "src/")
	if err != nil {
		return err
	}
	fmt.Printf("  agreement for src/: %s\n", rate)
	fmt.Printf("  %s\n", agreement.NewGate(0, 0).Allow(rate))

	// Record enough gated promotions for the scope to qualify. In a real
	// deployment this is weeks of ordinary reviewed work, which is the point.
	for i := 0; i < agreement.DefaultMinObservations; i++ {
		o := agreement.Observe("src/", ticket, "top", "top", clock.Add(time.Duration(i)*time.Second))
		if err := agreement.Record(mini.v, ticketO, o); err != nil {
			return err
		}
	}
	rate, err = agreement.RateFor(mini.v, "src/")
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

	section("done")
	fmt.Println("Every step above was a write to repository state. No cell ever told another")
	fmt.Println("cell what to do, and no process held a queue.")
	return nil
}

// demoCell is a cell plus the handles the demo needs to poke at it.
type demoCell struct {
	cell        *loop.Cell
	v           *varvigcli.Fake
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

	b := budget.Budget{InferenceDaily: 100, PerCallCost: 1, VerifyConcurrent: 2, StorageGB: 10, AttemptsDefault: 1}
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

	d := &demoCell{v: v, sw: sw, ledger: ledger, switchPath: switchPath,
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
		V:         v,
		Inference: runtime,
		Sandbox:   &sandbox.Fake{},
		Artifacts: &artifact.LocalCAS{Root: filepath.Join(work, id, "artifacts")},
		Ledger:    ledger,
		Upstream:  "upstream",
		Branch:    branch,
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
		V: v, Switch: sw, Gate: gate.Module{V: v},
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
	b := budget.Budget{InferenceDaily: 100, PerCallCost: 1, VerifyConcurrent: 2, StorageGB: 10, AttemptsDefault: 1}
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
