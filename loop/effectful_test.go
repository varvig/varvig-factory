package loop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/budget"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/effect"
	"github.com/varvig/varvig-factory/varvigcli"

	"github.com/varvig/varvig-factory/iface"
)

const effTicket = "c3feed0000000000000000000000000000000000000000000000000000000009"

var effClock = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// boardInterface is a stable interface hash for the tests.
// boardInterface publishes the board-fabrication schema into a factory's
// registry and returns its hash.
//
// It publishes rather than computing a hash because the effectful path now
// refuses an interface the registry does not hold — a hash nobody published
// describes nothing, and acting on it means ordering a shape no one here can
// state. A test that minted a bare hash would be testing a path a cell no longer
// takes.
func boardInterface(t *testing.T, f varvigcli.FactoryRepo) string {
	t.Helper()
	hash, err := iface.Publish(f, "pcb-fabrication@1", map[string]any{
		"gerber": "string", "quantity": "integer",
	})
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// effectCell builds a cell equipped to order boards: the capability declared, a
// lease held, an envelope above it, and a fake executor.
func effectCell(t *testing.T, amount cell.Money) (*Cell, *varvigcli.Fake, *effect.Fake) {
	t.Helper()
	v := varvigcli.NewFake("mini-a")
	fr, pr := varvigcli.Collapsed(v)
	ifaceHash := boardInterface(t, fr)
	spec := fmt.Sprintf("Order the prototype run.\nfactory-requires: effect=pcb-fabrication@1 interface=%s\nfactory-effect: {\"gerber\":\"rev-c\",\"quantity\":5}", ifaceHash)
	v.AddTicket(effTicket, spec, varvigcli.Scope{Reads: []string{"hardware"}, Writes: []string{"hardware"}}, "approved")

	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: effClock.Unix(),
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 500000, Unit: "EUR", Quantity: 100}},
	}
	if _, err := authority.PublishEnvelope(fr, env, ""); err != nil {
		t.Fatal(err)
	}
	lease := authority.Lease{
		CellID: "mini-a", Capability: "pcb-fabrication@1", Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: amount, Unit: "EUR", Quantity: 20, IssuedAt: effClock.Unix(),
	}
	if _, err := authority.PublishLease(fr, lease, ""); err != nil {
		t.Fatal(err)
	}

	capability := effect.Capability{ID: "pcb-fabrication@1", Interface: ifaceHash, Effectful: true}
	fake := effect.NewFake(capability, 32000, "EUR")
	// A ledger, because Once consults it for every ticket — including, as a
	// separate test asserts, effectful ones it must not gate.
	ledger, err := budget.NewLedger(
		budget.Budget{InferenceDaily: 1000, VerifyConcurrent: 1, StorageGB: 1, AttemptsDefault: 1, PerCallCost: 1},
		filepath.Join(t.TempDir(), "ledger.json"), effClock)
	if err != nil {
		t.Fatal(err)
	}
	c := &Cell{
		Capabilities: cell.Capabilities{
			CellID:  "mini-a",
			Effects: []cell.EffectCapability{{ID: "pcb-fabrication@1", Interface: ifaceHash}},
		},
		Factory:            fr,
		Project:            pr,
		Ledger:             ledger,
		Executors:          effect.Executors{fake},
		EffectAuthorizedBy: "overseer-a",
		EffectTTL:          3600,
		Now:                func() time.Time { return effClock },
		Log:                func(string) {},
	}
	return c, v, fake
}

func effectTicket(t *testing.T, v *varvigcli.Fake) claim.Ticket {
	t.Helper()
	spec, err := v.Spec(effTicket)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := v.Scope(effTicket)
	if err != nil {
		t.Fatal(err)
	}
	return claim.Ticket{ID: effTicket, Object: effTicket, Spec: spec, Scope: scope, Status: "approved"}
}

func TestATicketCanOrderAThing(t *testing.T) {
	c, v, fake := effectCell(t, 100000)
	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatalf("performing the effect: %v", err)
	}
	if !res.Done {
		t.Fatalf("the order did not happen: %+v", res)
	}
	if fake.Count() != 1 {
		t.Fatalf("the effect happened %d times, want exactly 1", fake.Count())
	}
	if res.ExternalRef == "" {
		t.Fatal("a completed order carries no external reference to look up")
	}

	// The lease was actually charged, and the hold converted rather than left.
	lease, _, err := authority.LoadLease(c.Factory, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Spent != 32000 || lease.Reserved != 0 {
		t.Fatalf("lease after settlement: spent=%s reserved=%s, want 320 and 0", lease.Spent, lease.Reserved)
	}

	// And the ticket itself records what it cost, so the question is answerable
	// where the work is rather than only under a reservation ref.
	notes, err := v.Notes(effTicket, cell.NoteEffect)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %d (err %v), want the one effect record", len(notes), err)
	}
	if !strings.Contains(string(notes[0].Payload), res.ExternalRef) {
		t.Fatalf("the note does not carry the external reference: %s", notes[0].Payload)
	}
}

func TestTheSameTicketNeverOrdersTwice(t *testing.T) {
	// The property everything else is in service of. A second pass over the same
	// ticket — a restart, a re-observed ticket, a rerun — must not re-order.
	c, v, fake := effectCell(t, 100000)
	ticket := effectTicket(t, v)

	first, err := c.performEffect(context.Background(), ticket)
	if err != nil || !first.Done {
		t.Fatalf("first pass: %+v (err %v)", first, err)
	}
	second, err := c.performEffect(context.Background(), ticket)
	if err != nil {
		t.Fatalf("second pass errored: %v", err)
	}
	if fake.Count() != 1 {
		t.Fatalf("the effect happened %d times across two passes, want 1", fake.Count())
	}
	// The second pass is told what happened, not merely refused.
	if second.ExternalRef != first.ExternalRef {
		t.Fatalf("the second pass did not report the original outcome: %+v", second)
	}
}

func TestAnUnknownOutcomeStaysPendingAndEscalates(t *testing.T) {
	// The case that matters most: the request left and nothing came back. The
	// cell does not know whether the order was placed, and every wrong answer
	// here is expensive.
	c, v, fake := effectCell(t, 100000)
	fake.ExecErr = errors.New("connection reset after the request was sent")

	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatalf("performing the effect: %v", err)
	}
	if !res.Unresolved || res.Done {
		t.Fatalf("an unknown outcome was resolved: %+v", res)
	}

	// The reservation stays pending, so it shows up for a principal to check.
	pending, err := effect.Pending(c.Project, "mini-a")
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d (err %v), want the one unresolved action", len(pending), err)
	}
	// The hold is NOT released: the money may be gone, and returning it would
	// let the cell spend it a second time.
	lease, _, err := authority.LoadLease(c.Factory, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Reserved != 32000 {
		t.Fatalf("reserved = %s, want the hold still standing at 320", lease.Reserved)
	}
	// And the cell does not try again on the next pass.
	if _, err := c.performEffect(context.Background(), effectTicket(t, v)); err != nil {
		t.Fatal(err)
	}
	if fake.Count() != 1 {
		t.Fatalf("an unresolved action was retried: executed %d times", fake.Count())
	}
}

func TestADefiniteRejectionReleasesTheHold(t *testing.T) {
	// "The vendor said no" and "we never heard back" lead to opposite decisions,
	// and only the first may return the money.
	c, v, fake := effectCell(t, 100000)
	fake.ExecErr = fmt.Errorf("%w: layer count not supported", effect.ErrRejected)

	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatalf("performing the effect: %v", err)
	}
	if res.Done || res.Unresolved || !res.Refused {
		t.Fatalf("a definite rejection was not recorded as one: %+v", res)
	}
	lease, _, err := authority.LoadLease(c.Factory, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Reserved != 0 || lease.Spent != 0 {
		t.Fatalf("a rejection left the lease at reserved=%s spent=%s", lease.Reserved, lease.Spent)
	}
	// Nothing pending: a confirmed rejection is resolved, and listing it as
	// unknown would bury the ones that really are.
	if p, err := effect.Pending(c.Project, "mini-a"); err != nil || len(p) != 0 {
		t.Fatalf("pending = %v (err %v), want empty", p, err)
	}
	// The key stays claimed, so the loop does not simply try again.
	if _, err := c.performEffect(context.Background(), effectTicket(t, v)); err != nil {
		t.Fatal(err)
	}
	if fake.Count() != 1 {
		t.Fatalf("a rejected action was retried: executed %d times", fake.Count())
	}
}

func TestAQuoteBeyondTheLeaseNeverReachesTheExecutor(t *testing.T) {
	// The refusal has to land before Execute, because after it the money is gone
	// regardless of what any check says.
	c, v, fake := effectCell(t, 10000) // lease of 100 against a quote of 320
	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatal(err)
	}
	if res.Done || !res.Refused {
		t.Fatalf("an order beyond the lease went through: %+v", res)
	}
	if fake.Count() != 0 {
		t.Fatal("the executor was called for an action that was not authorized")
	}
	if len(fake.Quoted) != 1 {
		t.Fatalf("quoted %d times, want 1: pricing must precede authorization", len(fake.Quoted))
	}
	// Nothing was held, and nothing is pending.
	lease, _, err := authority.LoadLease(c.Factory, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Reserved != 0 {
		t.Fatalf("a refused action held %s", lease.Reserved)
	}
}

func TestACellCannotAuthorizeItsOwnOrderThroughTheLoop(t *testing.T) {
	// §9.15 at the level that matters: not the library call, the wired cell.
	c, v, fake := effectCell(t, 100000)
	c.EffectAuthorizedBy = "mini-a"

	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatal(err)
	}
	if res.Done || !strings.Contains(res.Reason, "cannot authorize") {
		t.Fatalf("a cell authorized its own order: %+v", res)
	}
	if fake.Count() != 0 {
		t.Fatal("a self-authorized order reached the executor")
	}

	// An unset principal is refused on the same rule, not defaulted.
	c.EffectAuthorizedBy = ""
	if res, err := c.performEffect(context.Background(), effectTicket(t, v)); err != nil || res.Done {
		t.Fatalf("an unauthorized order went through: %+v (err %v)", res, err)
	}
	if fake.Count() != 0 {
		t.Fatal("an unauthorized order reached the executor")
	}
}

func TestATightenedEnvelopeStopsTheLoopOrdering(t *testing.T) {
	// §9.12 through the wired cell: the lease is untouched and still says 1000,
	// but the overseer has narrowed what they will stand behind.
	c, v, fake := effectCell(t, 100000)
	tight := authority.Envelope{
		Overseer: "overseer-a", SetAt: effClock.Unix(),
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 5000, Unit: "EUR", Quantity: 100}},
	}
	hash, err := v.ResolveRef(cell.EnvelopePrefix + "overseer-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.PublishEnvelope(c.Factory, tight, hash); err != nil {
		t.Fatal(err)
	}

	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatal(err)
	}
	if res.Done || fake.Count() != 0 {
		t.Fatalf("a tightened envelope did not stop the order: %+v", res)
	}
}

func TestACellWithNoExecutorDeclinesAndSaysSo(t *testing.T) {
	// The normal case for almost every cell. It must be a legible refusal, not a
	// crash and not silence.
	c, v, _ := effectCell(t, 100000)
	c.Executors = nil

	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatal(err)
	}
	if res.Done || !res.Refused {
		t.Fatalf("a cell with no executor did not refuse: %+v", res)
	}
	if !strings.Contains(res.Reason, "no executor") {
		t.Fatalf("the refusal does not name the cause: %s", res.Reason)
	}
	// And such a cell does not claim the ticket in the first place.
	if grants := c.effectGrants(); len(grants) != 0 {
		t.Fatalf("a cell with no executor reported grants: %v", grants)
	}
}

func TestGrantsNeedBothALeaseAndAnExecutor(t *testing.T) {
	c, v, _ := effectCell(t, 100000)
	if grants := c.effectGrants(); len(grants) != 1 {
		t.Fatalf("a fully equipped cell reported %d grants, want 1", len(grants))
	}

	// Executor but no lease: the cell can act and is not allowed to.
	if err := v.DeleteRef(mustLeaseRef(t), mustResolve(t, v, mustLeaseRef(t))); err != nil {
		t.Fatal(err)
	}
	if grants := c.effectGrants(); len(grants) != 0 {
		t.Fatalf("a cell with no lease reported grants: %v", grants)
	}
}

func mustLeaseRef(t *testing.T) string {
	t.Helper()
	name, err := cell.LeaseRef("mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func mustResolve(t *testing.T, v *varvigcli.Fake, name string) string {
	t.Helper()
	h, err := v.ResolveRef(name)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestASecondPassDoesNotEvenRequote(t *testing.T) {
	// The reservation ref is what guarantees one order. This is about the pass
	// before it: without a record of prior action the claim policy re-claims
	// every time, and for a quoted capability that means calling the vendor's
	// pricing API on every pass, forever.
	c, v, fake := effectCell(t, 100000)
	ticket := effectTicket(t, v)

	if c.priorEffect(ticket) != 0 {
		t.Fatal("a ticket nothing has acted on reported prior action")
	}
	if _, err := c.performEffect(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	quotesAfterFirst := len(fake.Quoted)

	if c.priorEffect(ticket) != 1 {
		t.Fatal("a completed effectful ticket did not report prior action")
	}
	// Drive the whole pass, so the claim policy is what does the skipping.
	rep, err := c.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Effects) != 0 {
		t.Fatalf("a second pass acted again: %+v", rep.Effects)
	}
	if len(fake.Quoted) != quotesAfterFirst {
		t.Fatalf("the vendor was re-quoted on a pass that had nothing to do (%d then %d)",
			quotesAfterFirst, len(fake.Quoted))
	}
	if fake.Count() != 1 {
		t.Fatalf("the effect happened %d times", fake.Count())
	}

	// A different payload is a different action, so it is not suppressed by the
	// first one's reservation — otherwise a genuine second order would vanish.
	other := ticket
	other.Spec = strings.Replace(ticket.Spec, `"quantity":5`, `"quantity":9`, 1)
	if c.priorEffect(other) != 0 {
		t.Fatal("a different order was treated as already done")
	}
}

func TestAConnectorServedCapabilityRoundTrips(t *testing.T) {
	// The whole exchange through the wired cell: the cell offers, a connector
	// takes and reports, and a later pass settles. The connector here is just
	// test code calling the protocol — which is the point, since a real one is a
	// separate process doing exactly this.
	c, v, fake := effectCell(t, 100000)
	ifaceHash := boardInterface(t, c.Factory)
	c.Connectors = map[string]bool{ifaceHash: true}

	res, err := c.performEffect(context.Background(), effectTicket(t, v))
	if err != nil {
		t.Fatalf("offering: %v", err)
	}
	if !res.Offered || res.Done {
		t.Fatalf("a connector-served capability was executed in-process: %+v", res)
	}
	if fake.Count() != 0 {
		t.Fatal("the in-process executor ran for a connector-served capability")
	}
	// The headroom is committed at the offer: a connector may pick it up at any
	// moment and the cell cannot take that back.
	held, _, err := authority.LoadLease(c.Factory, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if held.Reserved != 32000 {
		t.Fatalf("reserved = %s at the offer, want 320", held.Reserved)
	}

	// A connector, elsewhere.
	capability := effect.Capability{ID: "pcb-fabrication@1", Interface: ifaceHash, Effectful: true}
	awaiting, err := effect.Awaiting(c.Project, capability.Interface)
	if err != nil || len(awaiting) != 1 {
		t.Fatalf("awaiting = %+v (err %v)", awaiting, err)
	}
	taken, err := effect.Take(c.Project, "mini-a", awaiting[0].Key, "fab-connector", effClock.Unix()+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := effect.Report(c.Project, taken, "fab-connector", true, "PO-77", 34120, "2 layer", effClock.Unix()+60); err != nil {
		t.Fatal(err)
	}

	// A later pass settles it. Driven through Once, because a report lands long
	// after the pass that offered it and usually while the cell is doing
	// something else.
	rep, err := c.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var settled *EffectResult
	for i := range rep.Effects {
		if rep.Effects[i].ExternalRef == "PO-77" {
			settled = &rep.Effects[i]
		}
	}
	if settled == nil || !settled.Done {
		t.Fatalf("the report was not settled: %+v", rep.Effects)
	}
	after, _, err := authority.LoadLease(c.Factory, "mini-a", "pcb-fabrication@1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Spent != 34120 || after.Reserved != 0 {
		t.Fatalf("after settlement: spent=%s reserved=%s", after.Spent, after.Reserved)
	}
	// And it does not settle twice on the pass after that.
	again, err := c.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range again.Effects {
		if e.ExternalRef == "PO-77" {
			t.Fatalf("a settled report was settled again: %+v", e)
		}
	}
}
