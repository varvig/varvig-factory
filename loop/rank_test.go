package loop

import (
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/claim"
	"github.com/varvig/varvig-factory/varvigcli"
)

func rankCell(t *testing.T) (*Cell, *varvigcli.Fake) {
	t.Helper()
	v := varvigcli.NewFake("mini-a")
	return &Cell{
		Capabilities: cell.Capabilities{CellID: "mini-a"},
		V:            v,
		Log:          func(string) {},
	}, v
}

func ids(tickets []claim.Ticket) []string {
	out := make([]string, len(tickets))
	for i, t := range tickets {
		out[i] = t.ID
	}
	return out
}

const (
	tA = "1e20aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	tB = "1e20bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
	tC = "1e20cccccccccccc3333333333333333333333333333333333333333333333333333"
)

func TestCoreDecidesTheOrder(t *testing.T) {
	c, v := rankCell(t)
	listed := []claim.Ticket{{ID: tA}, {ID: tB}, {ID: tC}}
	v.SetRank(tC, tA, tB)

	got := ids(c.rank(listed))
	if want := []string{tC, tA, tB}; !equal(got, want) {
		t.Fatalf("order = %v, want core's %v", short3(got), short3(want))
	}
}

func TestRankingReordersAndNeverFilters(t *testing.T) {
	// `tickets rank` covers only scoped tickets. A ticket core does not mention
	// must keep its place rather than disappearing — becoming invisible would
	// silently change what the cell does, and the claim policy already has a
	// clear refusal for an unscoped ticket.
	c, v := rankCell(t)
	listed := []claim.Ticket{{ID: tA}, {ID: tB}, {ID: tC}}
	v.SetRank(tC) // only one is scoped

	got := ids(c.rank(listed))
	if len(got) != 3 {
		t.Fatalf("ranking dropped tickets: %v", short3(got))
	}
	if got[0] != tC {
		t.Fatalf("the ranked ticket did not come first: %v", short3(got))
	}
	// The unranked two keep their listed order behind it.
	if got[1] != tA || got[2] != tB {
		t.Fatalf("unranked tickets lost their listed order: %v", short3(got))
	}
}

func TestAnEmptyRankingLeavesTheOrderAlone(t *testing.T) {
	// An ordering hint that cannot be fetched must not stop the cell working.
	c, v := rankCell(t)
	listed := []claim.Ticket{{ID: tA}, {ID: tB}}

	// A core that ranks nothing — an unscoped repository — is not an error and
	// leaves the order alone.
	_ = v
	if got := ids(c.rank(listed)); !equal(got, []string{tA, tB}) {
		t.Fatalf("an empty ranking changed the order: %v", short3(got))
	}
}

func TestRankingNamesTicketsCoreDoesNotHave(t *testing.T) {
	// Core ranks from its own view, which may include a ticket this pass did not
	// observe. Naming one must not drop or duplicate anything.
	c, v := rankCell(t)
	listed := []claim.Ticket{{ID: tA}}
	v.SetRank(tB, tA)

	got := ids(c.rank(listed))
	if !equal(got, []string{tA}) {
		t.Fatalf("order = %v, want just the one observed ticket", short3(got))
	}
}

func TestShortIdMatching(t *testing.T) {
	// Core prints the digest's leading hex, past the multihash prefix.
	r := varvigcli.Ranked{ShortID: "aaaaaaaaaaaa"}
	if !r.Matches(tA) {
		t.Fatalf("%q did not match its own ticket", r.ShortID)
	}
	if r.Matches(tB) {
		t.Fatal("a short id matched the wrong ticket")
	}
	// A plain prefix is accepted too, since the abbreviation is core's choice.
	if !(varvigcli.Ranked{ShortID: "1e20aaaa"}).Matches(tA) {
		t.Fatal("a prefix form did not match")
	}
	if (varvigcli.Ranked{}).Matches(tA) {
		t.Fatal("an empty short id matched something")
	}
}

func TestARankFailureIsLoggedNotFatal(t *testing.T) {
	// The cell must keep working when the ordering cannot be fetched, and must
	// say why — a silently unordered cell looks identical to a correctly ordered
	// one until someone wonders why the important ticket never gets picked up.
	c, v := rankCell(t)
	var logged []string
	c.Log = func(s string) { logged = append(logged, s) }
	v.UnsupportedVerbs = map[string]bool{"tickets rank": true}

	if got := ids(c.rank([]claim.Ticket{{ID: tA}, {ID: tB}})); !equal(got, []string{tA, tB}) {
		t.Fatalf("a failed ranking changed the order: %v", short3(got))
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "listed order") {
		t.Fatalf("the failure was not reported: %v", logged)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func short3(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		if len(s) > 8 {
			s = s[:8]
		}
		out[i] = s
	}
	return out
}
