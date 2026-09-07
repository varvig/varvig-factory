package effect

import (
	"strings"
	"testing"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
)

var at = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return at }

// ifaceHash builds a real interface hash the way a registry would: hash the
// schema object and encode it as a multihash.
func ifaceHash(t *testing.T, schema any) string {
	t.Helper()
	labelled, err := cell.CanonicalHash(schema)
	if err != nil {
		t.Fatalf("hashing interface schema: %v", err)
	}
	mh, err := cell.ToMultihash(labelled)
	if err != nil {
		t.Fatalf("encoding interface hash: %v", err)
	}
	return mh
}

func fabrication(t *testing.T) Capability {
	t.Helper()
	return Capability{
		ID:        "pcb-fabrication@1",
		Interface: ifaceHash(t, map[string]any{"gerber": "string", "quantity": "integer"}),
		Effectful: true,
		CostModel: CostFixed,
	}
}

func heldLease(c Capability, amount float64) *authority.Lease {
	return &authority.Lease{
		CellID: "mini-a", Capability: c.ID, Overseer: "overseer-a",
		Envelope: "1e20abc", Amount: amount, Unit: "EUR", IssuedAt: at.Unix(),
	}
}

func order(t *testing.T, c Capability) Request {
	t.Helper()
	return Request{
		Capability:   c,
		Task:         "T-1042",
		Attempts:     1,
		Payload:      map[string]any{"gerber": "1e20deadbeef", "quantity": 5},
		Amount:       320,
		Quantity:     5,
		Unit:         "EUR",
		AuthorizedBy: "overseer-a",
	}
}

var online = authority.Sync{Configured: true, Reachable: true, At: at}

func TestAWellFormedOrderIsAllowed(t *testing.T) {
	c := fabrication(t)
	d := Check(order(t, c), "mini-a", heldLease(c, 1000), online, clock, 0)
	if !d.Allowed {
		t.Fatalf("a well-formed authorized order inside its lease was refused: %s", d.Error())
	}
	if d.Key == "" {
		t.Fatal("an allowed effectful action carries no idempotency key")
	}
}

// Test10_EffectfulSpeculationIsRejected is FACTORY.md §9.10: an effectful
// capability with --attempts 3 is *rejected*, not clamped to one.
func Test10_EffectfulSpeculationIsRejected(t *testing.T) {
	c := fabrication(t)
	req := order(t, c)
	req.Attempts = 3

	d := Check(req, "mini-a", heldLease(c, 1000), online, clock, 0)
	if d.Allowed {
		t.Fatal("three attempts at a board order were allowed; that is three invoices")
	}
	if !strings.Contains(d.Error(), ErrSpeculation.Error()) {
		t.Fatalf("the refusal does not name speculation: %s", d.Error())
	}
	// The distinction that matters: nothing here rewrites Attempts to 1. A clamp
	// would execute something other than what was asked for, on the one class of
	// action where that means a wrong order rather than a wasted GPU-hour.
	if req.Attempts != 3 {
		t.Fatalf("the request was clamped to %d attempts instead of rejected", req.Attempts)
	}
	// Zero is not "unspecified, assume one" either — an unset field on this path
	// is a task that never considered the question.
	req.Attempts = 0
	if Check(req, "mini-a", heldLease(c, 1000), online, clock, 0).Allowed {
		t.Fatal("an unspecified attempt count was treated as one")
	}
}

// Test11_Idempotency is FACTORY.md §9.11: the same logical action, retried after
// a mid-flight network failure, carries the same key and therefore executes once.
func Test11_Idempotency(t *testing.T) {
	c := fabrication(t)
	req := order(t, c)

	first := Check(req, "mini-a", heldLease(c, 1000), online, clock, 0)
	if !first.Allowed {
		t.Fatalf("first attempt refused: %s", first.Error())
	}

	// The network dies after the request left and before the response arrived.
	// The cell has no idea whether the order was placed. It reconstructs the same
	// intent — a fresh Request value, as a restarted process would build — and
	// must arrive at the same key.
	retry := order(t, c)
	// Map ordering must not matter, so hand the payload back with its keys
	// written in the other order.
	retry.Payload = map[string]any{"quantity": 5, "gerber": "1e20deadbeef"}
	second := Check(retry, "mini-a", heldLease(c, 1000), online, clock, 0)
	if second.Key != first.Key {
		t.Fatalf("the retry derived key %s, the original %s; the same action would be placed twice", second.Key, first.Key)
	}

	// A different quantity is a different action and must not collide with the
	// first — otherwise a genuine second order would be swallowed as a duplicate.
	other := order(t, c)
	other.Payload = map[string]any{"gerber": "1e20deadbeef", "quantity": 6}
	if k := Check(other, "mini-a", heldLease(c, 1000), online, clock, 0).Key; k == first.Key {
		t.Fatal("ordering a different quantity produced the same key; a real order would be dropped as a duplicate")
	}

	// Nor may a different task reuse a key, even for an identical payload.
	sameOrderOtherTask := order(t, c)
	sameOrderOtherTask.Task = "T-1043"
	if k := Check(sameOrderOtherTask, "mini-a", heldLease(c, 1000), online, clock, 0).Key; k == first.Key {
		t.Fatal("two tasks ordering the same board shared a key")
	}

	// Component boundaries are not smearable: a key must not be derivable by
	// shifting characters between the task id and the capability alias.
	shifted := c
	shifted.ID = "pcb-fabrication@"
	k1, err := IdempotencyKey("T-1042", c, req.Payload)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := IdempotencyKey("T-1042"+"1", shifted, req.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("the key is not length-prefixed; two different actions collide")
	}

	// A refusal still yields a key, so a caller that failed mid-flight can look
	// up whether the action already happened before deciding anything else.
	refused := order(t, c)
	refused.AuthorizedBy = ""
	if d := Check(refused, "mini-a", heldLease(c, 1000), online, clock, 0); d.Allowed || d.Key != first.Key {
		t.Fatalf("a refusal did not carry the action's key (allowed=%v key=%q)", d.Allowed, d.Key)
	}
}

// Test15_NoSelfAuthorization is FACTORY.md §9.15: a cell holding a factory key
// with promote rights still cannot authorize its own effectful action.
func Test15_NoSelfAuthorization(t *testing.T) {
	c := fabrication(t)
	req := order(t, c)
	req.AuthorizedBy = "mini-a" // the acting cell, promote rights or not

	d := Check(req, "mini-a", heldLease(c, 1000), online, clock, 0)
	if d.Allowed {
		t.Fatal("a cell authorized its own effectful action")
	}
	if !strings.Contains(d.Error(), ErrSelfAuthorization.Error()) {
		t.Fatalf("the refusal does not name self-authorization: %s", d.Error())
	}
	// It escalates rather than suggesting a retry: nothing the cell can do alone
	// changes this outcome. Promote rights move refs; they are not a licence to
	// spend money, and conflating the two turns a scoped repository credential
	// into a purchasing credential.
	if !d.Escalate {
		t.Fatal("self-authorization did not escalate to a higher principal")
	}

	// An absent principal is refused on the same rule rather than defaulting to
	// the acting cell.
	req.AuthorizedBy = ""
	if d := Check(req, "mini-a", heldLease(c, 1000), online, clock, 0); d.Allowed || !d.Escalate {
		t.Fatalf("an unauthorized effectful action was allowed or did not escalate: %s", d.Error())
	}
}

// Test16_InterfaceHashBinding is FACTORY.md §9.16: a capability that references
// only an alias is refused, and two capabilities sharing an alias but not a hash
// are different interfaces.
func Test16_InterfaceHashBinding(t *testing.T) {
	c := fabrication(t)

	aliasOnly := c
	aliasOnly.Interface = ""
	if err := aliasOnly.Validate(); err == nil {
		t.Fatal("a capability referencing only an alias validated")
	}
	if d := Check(order(t, aliasOnly), "mini-a", heldLease(c, 1000), online, clock, 0); d.Allowed {
		t.Fatal("an alias-only capability reference was allowed to act")
	}

	// A hash that is not an object hash is refused too: an arbitrary string would
	// bind to nothing and match nothing.
	bogus := c
	bogus.Interface = "definitely-not-a-hash"
	if err := bogus.Validate(); err == nil {
		t.Fatal("a capability naming a non-hash interface validated")
	}

	// Same alias, different schema. Two factories may hold the same alias without
	// agreeing who owns the name, so these must not match.
	impostor := Capability{
		ID:        c.ID,
		Interface: ifaceHash(t, map[string]any{"gerber": "string", "quantity": "integer", "ship_to": "string"}),
		Effectful: true,
	}
	if c.Matches(impostor) {
		t.Fatal("two interfaces sharing an alias but not a hash matched")
	}
	if !c.Matches(fabrication(t)) {
		t.Fatal("the same interface did not match itself")
	}
	// And an empty hash never matches, including another empty one — otherwise
	// two unbound references would agree with each other.
	if aliasOnly.Matches(aliasOnly) {
		t.Fatal("two unbound capability references matched")
	}

	// The hash is in the idempotency key, so re-pointing an alias at a new
	// interface cannot make a new action look like an old one.
	k1, err := IdempotencyKey("T-1042", c, map[string]any{"quantity": 5})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := IdempotencyKey("T-1042", impostor, map[string]any{"quantity": 5})
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("the interface hash does not reach the idempotency key")
	}
}

func TestOrdinaryCapabilitiesDoNotTravelThisPath(t *testing.T) {
	// Marking is explicit, and this package refuses anything unmarked: the whole
	// failure mode of the class is that an effectful capability looks ordinary,
	// so an unmarked one reaching here means someone's wiring is wrong.
	c := fabrication(t)
	c.Effectful = false
	if d := Check(order(t, c), "mini-a", heldLease(c, 1000), online, clock, 0); d.Allowed {
		t.Fatal("a capability not marked effectful was processed on the effectful path")
	}
}

func TestSpendComesFromTheActingCellsOwnLease(t *testing.T) {
	c := fabrication(t)

	// Another cell's lease is not spendable here, however much headroom it has.
	other := heldLease(c, 100000)
	other.CellID = "mini-b"
	d := Check(order(t, c), "mini-a", other, online, clock, 0)
	if d.Allowed {
		t.Fatal("a cell spent from another cell's lease")
	}
	if !strings.Contains(d.Error(), "mini-b") {
		t.Fatalf("the refusal does not name the lease holder: %s", d.Error())
	}

	// No lease at all escalates, even online: an effectful action spends from an
	// exclusive allocation, never from the shared envelope.
	if d := Check(order(t, c), "mini-a", nil, online, clock, 0); d.Allowed || !d.Escalate {
		t.Fatalf("an action with no lease was allowed or did not escalate: %s", d.Error())
	}

	// Beyond the lease escalates rather than falling back to the envelope.
	if d := Check(order(t, c), "mini-a", heldLease(c, 100), online, clock, 0); d.Allowed || !d.Escalate {
		t.Fatalf("spend beyond the lease was allowed or did not escalate: %s", d.Error())
	}
}

func TestOfflineOrderInsideItsLeaseIsAllowed(t *testing.T) {
	// The asymmetry §4.3b exists for: a fixed-price order within an outstanding
	// lease needs no connectivity, because the amount was committed when the
	// lease was issued.
	c := fabrication(t)
	offline := authority.Sync{Configured: true, Reachable: false, At: at.Add(-72 * time.Hour)}
	if d := Check(order(t, c), "mini-a", heldLease(c, 1000), offline, clock, 0); !d.Allowed {
		t.Fatalf("a disconnected cell was refused an order inside its own lease: %s", d.Error())
	}

	// A quoted capability is different in kind, not by policy: its price comes
	// from a service that is not reachable. Saying so makes the eventual external
	// failure legible.
	quoted := c
	quoted.CostModel = CostQuoted
	d := Check(order(t, quoted), "mini-a", heldLease(c, 1000), offline, clock, 0)
	if d.Allowed {
		t.Fatal("a quoted capability was priced with no reachable service")
	}
	if !strings.Contains(d.Error(), "not a policy") {
		t.Fatalf("the quoted refusal reads as a policy rule: %s", d.Error())
	}
	// Unknown cost models are refused rather than assumed fixed.
	weird := c
	weird.CostModel = "estimated"
	if err := weird.Validate(); err == nil {
		t.Fatal("an unknown cost model validated")
	}
}

func TestEveryUnmetRuleIsReported(t *testing.T) {
	// An operator about to spend money should see the full list, not the first
	// problem and then another round trip per remaining one.
	c := fabrication(t)
	req := order(t, c)
	req.Attempts = 4
	req.AuthorizedBy = "mini-a"
	req.Amount = 99999
	req.Unit = "USD"

	d := Check(req, "mini-a", heldLease(c, 1000), online, clock, 0)
	if d.Allowed {
		t.Fatal("a request breaking four rules was allowed")
	}
	if len(d.Refusals) < 4 {
		t.Fatalf("refusals = %d (%v), want one per broken rule", len(d.Refusals), d.Refusals)
	}
}

func TestUnlikeUnitsAreRefused(t *testing.T) {
	c := fabrication(t)
	req := order(t, c)
	req.Unit = "USD"
	d := Check(req, "mini-a", heldLease(c, 1000), online, clock, 0)
	if d.Allowed {
		t.Fatal("an order priced in USD spent from a EUR lease")
	}
	if !strings.Contains(d.Error(), "unlike units") {
		t.Fatalf("the refusal does not explain the unit mismatch: %s", d.Error())
	}
}

func TestIdempotencyKeyNeedsATask(t *testing.T) {
	c := fabrication(t)
	if _, err := IdempotencyKey("", c, nil); err == nil {
		t.Fatal("a key was derived with no task id")
	}
}
