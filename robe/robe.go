// Package robe is the responsibilities a cell wears, and the boundaries that
// make them safe to wear (FACTORY.md §5b.3).
//
// # Robes carry no authority
//
// This is the whole design and it is worth stating before anything else,
// because the obvious implementation is the wrong one. A robe looks like a
// permission — "the Provisioner is the one that may create cells" — and
// implementing it that way would mean a key per robe, a trust-store edit every
// time a robe moved, and a lifecycle to revoke one.
//
// It is not a permission. A robe makes a cell *inclined* to do certain work; it
// is a claim-policy input. What a cell may do comes from one enrolment scope
// granted once, unaffected by what it wears. Check each robe against that and
// none of them needs anything more:
//
//	Monitor      only reads
//	Ambassador   creates tickets, which any cell may propose
//	Procurement  proposes Profiles, which is unprivileged
//	Provisioner  acts, and needs infrastructure credentials and a lease —
//	             not elevated repository rights
//
// Two consequences follow, and both are load-bearing. Robe assignment is a
// **derived projection** over replicated state, so a cell that stops appearing
// stops wearing anything with no tombstone and no cleanup job. And a compromised
// cell key reaches its own factory namespace whatever it claims to wear.
//
// # What this package is not
//
// It holds no scheduler, no assignment protocol and no way for one cell to give
// another a robe. A cell wears what its own configuration says it wears, and
// every other cell reads that from the replicated capabilities object. Pushing
// a robe onto a peer would be authoritative assignment, which needs consensus
// this design deliberately does not build — the same argument that kept the
// delegation message out of §5.1.
package robe

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Wearer is one cell as the factory sees it.
//
// It carries the whole capabilities object rather than just the robes, so that
// "who wears what" and "who can do what" are two views of **one read**. Two
// answers derived from two reads taken a moment apart would be a factory that
// disagrees with itself about what it is.
type Wearer struct {
	CellID       string
	Capabilities cell.Capabilities
}

// Robes is what this cell wears.
func (w Wearer) Robes() []cell.Robe { return w.Capabilities.Robes }

// Projection is who wears what across the factory, derived from the
// capabilities objects in the coordination replica.
//
// Derived, never stored: there is no robe registry to keep in step, and a cell
// that has gone simply stops appearing here. That is the §5b.4b rule — state
// that can be derived needs no lifecycle — and it is why robes cost nothing to
// change and nothing to clean up.
type Projection struct {
	// Wearers is every cell that publishes capabilities, sorted by id, whether
	// or not it wears anything. A cell with no robes is still a member.
	Wearers []Wearer
}

// Wearing lists the cells wearing a robe, sorted.
//
// The answer may be empty and that is not an error: a factory with no
// Procurement robe is fully functional and simply does not grow (§5b.2), and a
// factory with no Ambassador simply has no external input surface.
func (p Projection) Wearing(r cell.Robe) []string {
	var out []string
	for _, w := range p.Wearers {
		for _, worn := range w.Robes() {
			if worn == r {
				out = append(out, w.CellID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Project reads every cell's published capabilities and derives who wears what.
//
// An unreadable capabilities object is reported rather than skipped, with the
// partial projection returned alongside: a cell whose advertisement cannot be
// read is a cell nobody can reason about, and silently leaving it out would
// make a factory that is half unreadable look like a smaller healthy one.
func Project(f varvigcli.FactoryRepo) (Projection, error) {
	refs, err := f.Refs()
	if err != nil {
		return Projection{}, err
	}
	var p Projection
	var bad []string
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, cell.CapabilitiesPrefix) || !strings.HasSuffix(r.Name, "/capabilities") {
			continue
		}
		body, err := f.ReadBlob(r.Hash)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		var caps cell.Capabilities
		if err := json.Unmarshal(body, &caps); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		p.Wearers = append(p.Wearers, Wearer{CellID: caps.CellID, Capabilities: caps})
	}
	sort.Slice(p.Wearers, func(i, j int) bool { return p.Wearers[i].CellID < p.Wearers[j].CellID })
	if len(bad) > 0 {
		return p, fmt.Errorf("robe: %d unreadable capabilities objects: %s", len(bad), strings.Join(bad, "; "))
	}
	return p, nil
}

// Grants is what wearing robes adds to a cell's authority.
//
// It returns nothing, always, and it exists to be called rather than to be
// useful: the one rule this package has is easiest to break by quietly adding a
// case, and a function whose whole body is a nil return is a place for that
// mistake to be visible. §9.20 calls it for every combination of robes.
//
// If this ever needs a parameter, the design has changed and §5b.3 has to
// change with it.
func Grants(robes []cell.Robe) []string { return nil }

// EnrolmentScope is the one bundle a cell is granted at enrolment (§5b.3),
// independent of every robe it wears.
//
// Returned as data rather than documented in prose so a test can assert on it,
// because "robes do not widen this" is the kind of claim that stays true only
// while something checks.
func EnrolmentScope(cellID string) []Scope {
	return []Scope{
		{Path: cell.CapabilitiesPrefix + cellID + "/", Rights: []string{"propose", "promote"},
			Why: "its own uncontested facts"},
		{Path: cell.LeasePrefix + cellID + "/", Rights: []string{"read"},
			Why: "it spends the lease, never writes it"},
		{Path: cell.Prefix, Rights: []string{"read"},
			Why: "the rest of the coordination repository"},
	}
}

// Scope is one path and what a cell may do there.
type Scope struct {
	Path   string
	Rights []string
	Why    string
}

// CanEnrol reports whether the key with this fingerprint could bring a new cell
// into the factory — which is to say, write another cell's authority.
//
// This is the key-chain question behind §9.20. A provisioned cell's ownership
// must root to the **owner**, never to the provisioner, and the way to be sure
// is not to check a configuration field saying so: it is that the provisioner's
// key is granted nothing that could have written the entry. A compromised
// provisioner that could mint cells would fork the trust root, and every cell it
// minted would look exactly like a legitimate one.
//
// So the answer is about scope and rights in the trust store, and nothing about
// robes: a cell scoped to its own namespace cannot enrol anybody, whatever it
// wears.
func CanEnrol(entries []varvigcli.TrustEntry, fingerprint string) bool {
	for _, e := range entries {
		if e.Fingerprint != fingerprint {
			continue
		}
		// Enrolling means writing a *different* cell's namespace, so the
		// question is whether this key reaches the cells prefix at all rather
		// than whether it reaches any particular cell.
		if e.Can("promote", cell.CapabilitiesPrefix) {
			return true
		}
	}
	return false
}

// Providers is the capability view of the same projection — what this factory
// can do, as opposed to who is responsible for what.
func (p Projection) Providers() Providers {
	out := Providers{}
	for _, w := range p.Wearers {
		out.Cells = append(out.Cells, w.Capabilities)
	}
	return out
}
