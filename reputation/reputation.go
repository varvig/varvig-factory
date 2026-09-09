// Package reputation derives what a cell's record actually is, from what it has
// done rather than from what it says about itself.
//
// A cell's capabilities object is a *claim*: it says which models it holds and
// which roles it takes, and nothing in the system checks it. That is deliberate
// and fine — a cell that lies about its model produces work that is worse, and
// the work is what gets scored. What was missing is anyone doing the scoring per
// cell. The agreement metric is derived, but per *scope*: it answers "is the
// scorer calibrated in src/", not "is this cell's work worth attempting".
//
// # Derived from attempts and promotions, and nothing else
//
// A cell's standing here is: of the attempts it made at tasks that were later
// promoted, how often was its attempt the one promoted. Both halves come from
// repository state a cell cannot write on another's behalf — attempt refs are
// namespaced by cell id and immutable, and a promotion observation names the
// change that actually moved.
//
// # What it is deliberately not wired into
//
// Nothing here feeds claim policy. A cell that consulted reputation to decide
// whether to attempt would be ordering work by a quality judgement, and ordering
// work is varvig's job — the prohibition against a second scheduler (§10.1) does
// not have an exception for a well-meaning one. Worse, it would compound: a cell
// with a poor early record would attempt less, and so have less record.
//
// So this is a *report*. An overseer sizing a lease, a human deciding which
// cells to keep running, a factory deciding where to send a hard ticket — those
// are decisions made by someone who can see the whole picture, and this gives
// them the number rather than acting on it.
package reputation

import (
	"encoding/json"
	"sort"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/varvigcli"
)

// Standing is one cell's derived record.
type Standing struct {
	CellID string `json:"cell_id"`
	// Attempts is how many attempts this cell made at tasks that were promoted.
	//
	// Attempts at tasks nobody has promoted yet are excluded: they are not
	// failures, they are unfinished, and counting them as losses would rank a
	// cell down for working on things that are still open.
	Attempts int `json:"attempts"`
	// Promoted is how many of those were the attempt that moved.
	Promoted int `json:"promoted"`
	// Tasks is how many distinct tasks it competed on.
	Tasks int `json:"tasks"`
}

// Rate is the share of contested attempts this cell won, or -1 when it has not
// competed.
//
// -1 rather than 0: "no record" and "a record of losing" are opposite answers,
// and a new cell reading as the worst possible cell is how a metric becomes a
// barrier to entry.
func (s Standing) Rate() float64 {
	if s.Attempts == 0 {
		return -1
	}
	return float64(s.Promoted) / float64(s.Attempts)
}

// Derive reads attempts and promotion observations and returns each cell's
// standing, best first, ties broken by cell id so the order is stable.
//
// It reads the project replica: attempts and promotions are facts about one
// codebase, and a cell's record in a codebase it has never touched is not a
// record of zero, it is silence.
func Derive(p varvigcli.ProjectRepo) ([]Standing, error) {
	obs, err := agreement.Observations(p)
	if err != nil {
		return nil, err
	}
	// promoted[task] is the change that moved for that task.
	promoted := map[string]string{}
	for _, o := range obs {
		if o.Task != "" && o.PromotedAttempt != "" {
			promoted[o.Task] = o.PromotedAttempt
		}
	}
	if len(promoted) == 0 {
		return nil, nil
	}

	refs, err := p.Refs()
	if err != nil {
		return nil, err
	}
	type key struct{ cellID, task string }
	seen := map[key]bool{}
	byCell := map[string]*Standing{}

	for _, r := range refs {
		cellID, task, _, perr := cell.ParseAttemptRef(r.Name)
		if perr != nil {
			continue
		}
		if _, contested := promoted[task]; !contested {
			continue
		}
		st := byCell[cellID]
		if st == nil {
			st = &Standing{CellID: cellID}
			byCell[cellID] = st
		}
		st.Attempts++
		if !seen[key{cellID, task}] {
			seen[key{cellID, task}] = true
			st.Tasks++
		}

		// The attempt object names the change it produced; the promotion names
		// the change that moved. Matching on the change rather than on the
		// attempt ref is what makes this work when a peer promoted another
		// cell's attempt, which is the ordinary case in a flat factory.
		att, aerr := loadAttempt(p, r.Hash)
		if aerr == nil && att.Change != "" && att.Change == promoted[task] {
			st.Promoted++
		}
	}

	out := make([]Standing, 0, len(byCell))
	for _, st := range byCell {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rate() != out[j].Rate() {
			return out[i].Rate() > out[j].Rate()
		}
		return out[i].CellID < out[j].CellID
	})
	return out, nil
}

// Of returns one cell's standing, or a zero Standing (Rate -1) when it has no
// record here.
func Of(p varvigcli.ProjectRepo, cellID string) (Standing, error) {
	all, err := Derive(p)
	if err != nil {
		return Standing{CellID: cellID}, err
	}
	for _, s := range all {
		if s.CellID == cellID {
			return s, nil
		}
	}
	return Standing{CellID: cellID}, nil
}

func loadAttempt(p varvigcli.ProjectRepo, hash string) (cell.Attempt, error) {
	var a cell.Attempt
	body, err := p.ReadBlob(hash)
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(body, &a)
}
