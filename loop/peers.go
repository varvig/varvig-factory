package loop

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"

	"github.com/varvig/varvig-factory/varvigcli"
)

// Peers is a rendezvous set: the addresses a cell may dial to sync one
// repository.
//
// It is a *set*, not a list in priority order, and that is the whole point.
// FACTORY.md §3.0 says any member may act as a rendezvous, several at once —
// not a role, not a coordinator, not an upstream. A cell reads nothing from a
// peer that another peer could not have served, so no member is privileged and
// the factory keeps working when any particular one is unreachable.
//
// # Every peer is contacted, not the first that answers
//
// Stopping at the first success would be cheaper and wrong. Peer B may hold
// attempts, evidence or a lease that peer A has never seen; taking A's answer
// and stopping means relying on A to relay the rest, which makes A a
// coordinator in effect however the config describes it. So a pass contacts
// every peer in the set, in both directions.
//
// # The order is shuffled every pass
//
// Reserved-ref replication takes what we lack and *reports* rather than
// overwrites when both sides hold a ref at unrelated values. So on a contested
// claim, whichever peer is contacted first is the one whose version we adopt. A
// fixed order would make the first-listed peer systematically win — a privilege
// arriving by the back door, through nothing more than the order someone typed
// addresses into a config file. Shuffling removes it.
type Peers []string

// shuffled returns the set in a random order, so no member is systematically
// first. The source is injectable because a test that cannot fix the order
// cannot assert what a pass did.
func (p Peers) shuffled(shuffle func(n int, swap func(i, j int))) Peers {
	out := append(Peers(nil), p...)
	if shuffle == nil {
		shuffle = rand.Shuffle
	}
	shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// visited records what contacting a rendezvous set achieved.
type visited struct {
	// answered counts the peers that completed the exchange.
	answered int
	// reached counts the peers that were dialled successfully, whether or not
	// everything they were sent applied.
	reached int
	// failures describes what went wrong, per peer.
	failures []string
}

// unreachable reports whether the whole set was out of contact.
//
// The distinction between *reached* and *answered* is load-bearing. varvig's
// head push moves under a force-with-lease against one tracking ref, so with
// several peers at most one can accept a head push and the rest are refused —
// every pass, as a matter of course (FEDERATION.md §6). Those peers were
// reached: notes and reserved refs replicated to them, the authority state
// arrived, only the branch disagreed. Counting that as unreachable would mark
// trust stale and stop promotion for a reason that has nothing to do with
// trust.
//
// So only a failure to make contact at all counts here.
func (v visited) unreachable() bool { return v.reached == 0 }

// visit contacts every peer in the set and reports what happened.
//
// A set with no members is not a failure and not unreachable: a single-cell
// deployment has nothing to be disconnected from, and treating it as offline
// would apply the tighter offline budget forever (§4.3b). The caller
// distinguishes that case by the set being empty, not by the result.
func (c *Cell) visit(peers Peers, what string, do func(addr string) error) visited {
	var v visited
	for _, addr := range peers.shuffled(c.Shuffle) {
		err := do(addr)
		switch {
		case err == nil:
			v.reached++
			v.answered++
		case errors.Is(err, varvigcli.ErrUnreachable):
			v.failures = append(v.failures, fmt.Sprintf("%s: unreachable", addr))
		default:
			// We got there and exchanged state; something did not fully apply.
			// A refused head compare-and-swap is the ordinary example, and it
			// is not a reachability problem.
			v.reached++
			v.failures = append(v.failures, fmt.Sprintf("%s: %v", addr, err))
		}
	}
	if len(v.failures) > 0 {
		c.logf("%s: %d of %d peers reached, %d fully synced (%s)",
			what, v.reached, len(peers), v.answered, strings.Join(v.failures, "; "))
	}
	return v
}
