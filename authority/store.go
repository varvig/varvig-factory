package authority

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// This file is the whole of authority's contact with a repository, and it is
// deliberately small: an envelope and a lease are objects, and their refs are
// CAS-updated like every other ref in the system. Nothing here needs a verb
// varvig does not already have, which is the point — a spend model that
// required core changes would be a spend model nobody could deploy.
//
// # Everything here is the factory-coordination replica, and nothing else
//
// Envelopes and leases answer "what may this cell spend", and that question has
// exactly one authoritative answer per factory. Writing them into a project
// repository would give every project its own plausible copy, and the sum of
// those copies would exceed the envelope with nothing able to notice — so every
// function in this file takes a FactoryRepo, and handing it a project replica is
// a compile error rather than a discovery made while reconciling a bill.

// PublishEnvelope writes an envelope object and points the overseer's ref at it.
//
// oldHash is the value the caller read, so a concurrent change is a refused swap
// rather than a silent overwrite. Pass "" to assert the ref does not yet exist.
func PublishEnvelope(f varvigcli.FactoryRepo, e Envelope, oldHash string) (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	name, err := cell.EnvelopeRef(e.Overseer)
	if err != nil {
		return "", err
	}
	return publish(f, name, e, oldHash)
}

// LoadEnvelope reads an overseer's envelope, returning it with the object hash it
// was read at — which is what a later PublishEnvelope needs to CAS against.
//
// A missing envelope is varvigcli.ErrNoRef and not an empty Envelope: no
// envelope means no authority to spend, and an empty value would read as
// "validated, with no ceilings", which §8.1 says is malformed.
func LoadEnvelope(f varvigcli.FactoryRepo, overseer string) (Envelope, string, error) {
	name, err := cell.EnvelopeRef(overseer)
	if err != nil {
		return Envelope{}, "", err
	}
	var e Envelope
	hash, err := load(f, name, &e)
	if err != nil {
		return Envelope{}, "", err
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, hash, fmt.Errorf("%s: %w", name, err)
	}
	return e, hash, nil
}

// PublishLease writes a lease object and points the cell's lease ref at it. It is
// how an overseer issues a lease and, with the hash from LoadLease, how a cell
// settles spend against one.
func PublishLease(f varvigcli.FactoryRepo, l Lease, oldHash string) (string, error) {
	if err := l.Validate(); err != nil {
		return "", err
	}
	name, err := cell.LeaseRef(l.CellID, l.Capability)
	if err != nil {
		return "", err
	}
	return publish(f, name, l, oldHash)
}

// LoadLease reads one cell's lease for one capability, with the hash it was read
// at. A cell with no lease for a capability gets varvigcli.ErrNoRef, which
// §8.2 rule 4 turns into a refusal rather than a fallback.
func LoadLease(f varvigcli.FactoryRepo, cellID, capability string) (Lease, string, error) {
	name, err := cell.LeaseRef(cellID, capability)
	if err != nil {
		return Lease{}, "", err
	}
	var l Lease
	hash, err := load(f, name, &l)
	if err != nil {
		return Lease{}, "", err
	}
	if err := l.Validate(); err != nil {
		return Lease{}, hash, fmt.Errorf("%s: %w", name, err)
	}
	return l, hash, nil
}

// Leases lists every outstanding lease in the repository, or those of one cell
// when cellID is non-empty.
//
// This is the call that answers "what am I exposed to": feed the result to
// Exposure. It reads every cell's leases and not only the local one, because the
// sum is the number that matters (§8.1) and a cell can only see its own share.
//
// A malformed lease is reported rather than skipped. Silently dropping one would
// understate exposure, which is the wrong direction to be wrong in.
func Leases(f varvigcli.FactoryRepo, cellID string) ([]Lease, error) {
	refs, err := f.Refs()
	if err != nil {
		return nil, err
	}
	prefix := cell.LeasePrefix
	if cellID != "" {
		if err := cell.CheckID(cellID); err != nil {
			return nil, err
		}
		prefix += cellID + "/"
	}

	var out []Lease
	var bad []string
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		body, err := f.ReadBlob(r.Hash)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		var l Lease
		if err := json.Unmarshal(body, &l); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		if err := l.Validate(); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		holder, capability, perr := cell.ParseLeaseRef(r.Name)
		if perr != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", r.Name, perr))
			continue
		}
		// The ref path and the object must agree. A lease object under another
		// cell's ref is either a bug or an attempt to borrow authority, and
		// either way summing it under the wrong holder would misreport who can
		// spend what.
		if holder != l.CellID || capability != l.Capability {
			bad = append(bad, fmt.Sprintf("%s: names %s/%s but the object says %s/%s",
				r.Name, holder, capability, l.CellID, l.Capability))
			continue
		}
		out = append(out, l)
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("authority: %d unreadable lease refs: %s", len(bad), strings.Join(bad, "; "))
	}
	return out, nil
}

// Reclaim deletes a lease ref, and refuses unless the lease is reclaimable.
//
// The refusal is the feature. A lease with any recorded spend is never reclaimed
// (§8.1), because the holder may have placed an order it has not yet reported,
// and reclaiming there is how a double-spend happens. `reclaim_after` passing
// makes the lease *reportable*, not collectable.
func Reclaim(f varvigcli.FactoryRepo, l Lease, oldHash string, now func() int64) error {
	if l.Spent > 0 || l.Ordered > 0 {
		return fmt.Errorf("authority: the lease %s has recorded spend; it is reported to the overseer, not reclaimed, because the cell may hold an order it has not yet reported", l)
	}
	if l.ReclaimAfter > 0 && now() < l.ReclaimAfter {
		return fmt.Errorf("authority: the lease %s is inside its reclaim timeout", l)
	}
	name, err := cell.LeaseRef(l.CellID, l.Capability)
	if err != nil {
		return err
	}
	return f.DeleteRef(name, oldHash)
}

func publish(f varvigcli.FactoryRepo, name string, payload any, oldHash string) (string, error) {
	body, err := cell.Canonical(payload)
	if err != nil {
		return "", err
	}
	id, err := f.PutBlob(body)
	if err != nil {
		return "", err
	}
	if err := f.UpdateRef(name, id, oldHash); err != nil {
		if errors.Is(err, varvigcli.ErrCAS) {
			// Naming the ref matters here: the caller's next step is to re-read
			// and re-apply, and a bare "compare-and-swap failed" from three
			// possible refs sends them looking in the wrong place.
			return "", fmt.Errorf("authority: %s changed under us; re-read it and re-apply: %w", name, err)
		}
		return "", err
	}
	return id, nil
}

func load(f varvigcli.FactoryRepo, name string, into any) (string, error) {
	hash, err := f.ResolveRef(name)
	if err != nil {
		return "", err
	}
	body, err := f.ReadBlob(hash)
	if err != nil {
		return hash, err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return hash, fmt.Errorf("authority: %s does not hold a well-formed object: %w", name, err)
	}
	return hash, nil
}
