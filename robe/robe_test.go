package robe

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

func factoryWith(t *testing.T, caps ...cell.Capabilities) varvigcli.FactoryRepo {
	t.Helper()
	v := varvigcli.NewFake("factory")
	f := varvigcli.FactoryRepo{Varvig: v}
	for _, c := range caps {
		c.Normalize()
		if err := c.Validate(); err != nil {
			t.Fatalf("fixture %s is invalid: %v", c.CellID, err)
		}
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		id, err := f.PutBlob(body)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := cell.CapabilitiesRef(c.CellID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.UpdateRef(ref, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func policyCell(id string, robes ...cell.Robe) cell.Capabilities {
	return cell.Capabilities{
		CellID: id,
		Roles:  []cell.Role{cell.RoleBuild, cell.RoleVerify},
		Build:  []string{"go"},
		// A policy cell has no model of its own: robes, resolution and
		// procurement are all judgment a cell makes from repository state, and
		// none of them needs inference. Tier none is the honest fixture.
		Inference: cell.Inference{Tier: cell.TierNone},
		Robes:     robes,
	}
}

// Test20_ProvisionedOwnership is §9.20: a cell created by a provisioner roots
// to the owner, not the provisioner — asserted on the key chain, not on
// configuration.
//
// The reason the assertion has to be on the key chain is that configuration can
// say anything. A provisioner that minted cells loyal to itself would write
// exactly the same config as one that did not; what differs is whether its key
// could have written the new cell's authority at all.
//
// So the check is scope and rights in the trust store, and the answer has
// nothing to do with robes: a cell scoped to its own namespace cannot enrol
// anybody, whatever it wears. A compromised provisioner forks the trust root
// only if somebody first granted it rights it has no reason to hold.
func Test20_ProvisionedOwnership(t *testing.T) {
	owner := varvigcli.TrustEntry{
		Fingerprint: "SHA256:owner", Name: "owner", Scope: "/",
		Rights: []string{"propose", "promote"},
	}
	provisioner := varvigcli.TrustEntry{
		Fingerprint: "SHA256:prov", Name: "prov-a", Scope: cell.CapabilitiesPrefix + "prov-a/",
		Rights: []string{"propose", "promote"},
	}
	// The cell the provisioner brought up. Its entry exists, and the question
	// is who could have written it.
	provisioned := varvigcli.TrustEntry{
		Fingerprint: "SHA256:new", Name: "new-b", Scope: cell.CapabilitiesPrefix + "new-b/",
		Rights: []string{"propose", "promote"},
	}
	store := []varvigcli.TrustEntry{owner, provisioner, provisioned}

	if !CanEnrol(store, owner.Fingerprint) {
		t.Fatal("the owner cannot enrol a cell; then nobody could have created this factory")
	}
	if CanEnrol(store, provisioner.Fingerprint) {
		t.Fatal("the provisioner can write another cell's authority: a compromised one would mint cells loyal to itself and fork the trust root")
	}
	if CanEnrol(store, provisioned.Fingerprint) {
		t.Fatal("a provisioned cell can enrol further cells, so growth would compound outside the owner's reach")
	}

	// And wearing the robe changes none of it. This is the part that would be
	// easy to get wrong by making the robe the thing that grants the right.
	f := factoryWith(t, policyCell("prov-a", cell.RobeProvisioner), policyCell("new-b"))
	p, err := Project(f)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Wearing(cell.RobeProvisioner); len(got) != 1 || got[0] != "prov-a" {
		t.Fatalf("projection = %v, want [prov-a]", got)
	}
	if CanEnrol(store, provisioner.Fingerprint) {
		t.Fatal("wearing the provisioner robe granted enrolment rights; robes carry no authority")
	}
}

// Test20b_RobesCarryNoAuthority is the general form of the rule §5b.3 states
// and §9.20 depends on.
//
// Worth its own vector because the rule is easiest to break by addition: every
// robe has an obvious thing it "should" be allowed to do, and granting it there
// looks like a small convenience rather than the design change it is.
func Test20b_RobesCarryNoAuthority(t *testing.T) {
	all := cell.Robes()
	for _, r := range all {
		if got := Grants([]cell.Robe{r}); len(got) != 0 {
			t.Fatalf("robe %q grants %v; robes are a claim-policy input, not a permission", r, got)
		}
	}
	if got := Grants(all); len(got) != 0 {
		t.Fatalf("wearing every robe grants %v", got)
	}

	// A cell's scope is one bundle granted at enrolment, and it is the same
	// bundle whether it wears everything or nothing.
	bare := EnrolmentScope("mini-a")
	if len(bare) == 0 {
		t.Fatal("a cell is granted nothing at enrolment, so it could not publish its own capabilities")
	}
	for _, s := range bare {
		if strings.HasPrefix(s.Path, cell.CapabilitiesPrefix) && !strings.Contains(s.Path, "mini-a") {
			t.Fatalf("enrolment scope %q reaches another cell's namespace", s.Path)
		}
	}
}

// Test19_ResolutionDeterminism is §9.19: every cell computes the same provider
// match from the same replicated state.
//
// The property matters because resolution runs locally on every cell with no
// coordinator (§5b.1). If two cells could disagree, a flat factory would need
// an election to settle which answer counts, and the coordinator this design
// exists to avoid would arrive through the back door.
func Test19_ResolutionDeterminism(t *testing.T) {
	iface := "1220" + strings.Repeat("ab", 32)
	other := "1220" + strings.Repeat("cd", 32)

	provider := func(id, hash string) cell.Capabilities {
		c := policyCell(id)
		c.Effects = []cell.EffectCapability{{ID: "pcb-fabrication@1", Interface: hash, CostModel: cell.CostFixed}}
		return c
	}
	// Written in one order here and read back in whatever order the refs come.
	f := factoryWith(t,
		provider("zeta", iface), policyCell("alpha"), provider("mid", iface), provider("beta", other))

	first, err := Project(f)
	if err != nil {
		t.Fatal(err)
	}
	got := Resolve(first.Providers(), iface)
	if want := []string{"mid", "zeta"}; strings.Join(got.Providers, ",") != strings.Join(want, ",") {
		t.Fatalf("providers = %v, want %v", got.Providers, want)
	}

	// Ten more reads, standing in for ten more cells: same state, same answer,
	// same order. Sorted output is what makes "the same answer" checkable at
	// all — two cells agreeing on a set but not its order would still disagree
	// about which provider to pick first.
	for i := 0; i < 10; i++ {
		p, err := Project(f)
		if err != nil {
			t.Fatal(err)
		}
		again := Resolve(p.Providers(), iface)
		if strings.Join(again.Providers, ",") != strings.Join(got.Providers, ",") {
			t.Fatalf("read %d resolved to %v, the first read to %v", i, again.Providers, got.Providers)
		}
	}

	// Providers is an exported type any caller may build, and resolution must
	// not inherit its input's order. Project happens to sort its wearers, so a
	// vector that only ever goes through Project would pass with no ordering
	// guarantee in Resolve at all — and the first caller assembling Providers
	// by hand would be the one to find out.
	hand := Providers{Cells: []cell.Capabilities{
		provider("zeta", iface), provider("mid", iface), provider("alpha-p", iface)}}
	if byHand := Resolve(hand, iface); strings.Join(byHand.Providers, ",") != "alpha-p,mid,zeta" {
		t.Fatalf("hand-built providers resolved to %v, in input order rather than sorted", byHand.Providers)
	}

	// Matching is on the hash, never the alias: beta advertises the same alias
	// for a different interface and must not be returned.
	for _, p := range got.Providers {
		if p == "beta" {
			t.Fatal("a provider was matched by alias; two factories may use one alias for different interfaces")
		}
	}

	// An unmet requirement resolves to nothing, immediately and without error.
	// Resolution must never block on procurement.
	if miss := Resolve(first.Providers(), "1220"+strings.Repeat("ef", 32)); miss.Met() {
		t.Fatal("an interface nothing provides resolved to a provider")
	}
}

// Test21_GrowthBound is §9.21: procurement cannot push the factory past the
// total-cell cap or the envelope, including by creating cells that create
// cells.
func Test21_GrowthBound(t *testing.T) {
	iface := "1220" + strings.Repeat("ab", 32)
	unmet := []Unmet{
		{Interface: iface, Alias: "pcb-fabrication@1", Task: "t1"},
		{Interface: iface, Alias: "pcb-fabrication@1", Task: "t2"},
	}
	env := authority.Envelope{
		Overseer: "overseer-a", SetAt: 1,
		Ceilings: []authority.Ceiling{{Capability: "pcb-fabrication@1", Spend: 100000, Unit: "EUR"}},
	}
	two := Providers{Cells: []cell.Capabilities{policyCell("a"), policyCell("b")}}

	// With no cap set, nothing is authorized. A factory that has not been told
	// how large it may become has not authorized becoming larger.
	if _, err := (Procurement{}).Propose(two, unmet, env); err == nil {
		t.Fatal("growth was proposed with no total-cell cap configured")
	}

	// Room for one more: one proposal, not one per unmet requirement.
	got, err := Procurement{TotalCells: 3}.Propose(two, unmet, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Wanted != 2 {
		t.Fatalf("proposals = %+v, want one for %d unmet requirements", got, len(unmet))
	}

	// At the cap, nothing is proposed — and the refusal says which bound, so an
	// operator is not left wondering whether procurement simply found nothing.
	if _, err := (Procurement{TotalCells: 2}).Propose(two, unmet, env); err == nil {
		t.Fatal("a factory at its cap proposed growth")
	} else if !strings.Contains(err.Error(), "2 of 2") {
		t.Fatalf("the refusal does not name the bound: %v", err)
	}

	// Recursion-awareness: cells a previous round created count. A factory that
	// grew from two to three cannot then treat itself as having room for a
	// fourth under a cap of three, which is the "cells that create cells" case.
	three := Providers{Cells: append(append([]cell.Capabilities{}, two.Cells...), policyCell("grown-c"))}
	if _, err := (Procurement{TotalCells: 3}).Propose(three, unmet, env); err == nil {
		t.Fatal("a cell created by a previous round did not count toward the cap")
	}
	if !(Procurement{TotalCells: 3}).WouldExceed(three, 1) {
		t.Fatal("WouldExceed disagrees with Propose about the same cap")
	}

	// A malformed envelope is not an unlimited one.
	if _, err := (Procurement{TotalCells: 9}).Propose(two, unmet, authority.Envelope{Overseer: "overseer-a", SetAt: 1}); err == nil {
		t.Fatal("growth was proposed against an envelope that bounds nothing")
	}

	// And procurement proposes; it never provisions.
	if err := (Procurement{TotalCells: 9}).Provision(Proposal{Interface: iface}); err == nil {
		t.Fatal("procurement provisioned a cell; deciding to spend and executing must be different principals")
	}
}
