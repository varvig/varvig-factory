package main

import (
	"fmt"
	"strings"

	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/robe"
)

// cmdRobes prints who wears what across the factory, derived from the
// replicated capabilities objects.
//
// Read-only, and there is deliberately no verb here that assigns one. A cell
// wears what its own configuration says it wears; a command that pushed a robe
// onto a peer would be authoritative assignment, which needs consensus this
// design does not build. To change what this cell wears, edit its `robes` and
// republish.
func cmdRobes(args []string) error {
	f, err := parseFlags(args, nil, []string{"scope"})
	if err != nil {
		return err
	}
	cfg, built, err := load(f)
	if err != nil {
		return err
	}

	if f.bools["scope"] {
		// What this cell may do, which is the question the projection below
		// invites and must not be allowed to answer wrongly.
		fmt.Printf("%s is granted, at enrolment and regardless of robes:\n", cfg.CellID)
		for _, s := range robe.EnrolmentScope(cfg.CellID) {
			fmt.Printf("  %-40s %-18s %s\n", s.Path, strings.Join(s.Rights, ","), s.Why)
		}
		return nil
	}

	p, err := robe.Project(built.Factory)
	if err != nil && len(p.Wearers) == 0 {
		// Nothing was read at all, so there is no projection to qualify. Saying
		// "no cell publishes capabilities here yet" would be a claim about the
		// factory when the truth is a claim about this cell's replica.
		return err
	}
	if err != nil {
		// A partial read is reported and then printed anyway: a cell whose
		// advertisement cannot be read is a cell nobody can reason about, and a
		// half-unreadable factory must not print as a smaller healthy one — but
		// what did read is still what the other cells see.
		fmt.Printf("warning: %v\n\n", err)
	}
	if len(p.Wearers) == 0 {
		fmt.Println("no cell publishes capabilities here yet")
		return nil
	}
	for _, w := range p.Wearers {
		worn := "—"
		if r := w.Robes(); len(r) > 0 {
			parts := make([]string, 0, len(r))
			for _, one := range r {
				parts = append(parts, string(one))
			}
			worn = strings.Join(parts, ", ")
		}
		fmt.Printf("%-20s %s\n", w.CellID, worn)
	}
	// Empty robes are stated because they are not a fault. A factory with no
	// procurement robe is fully functional and simply does not grow; one with
	// no ambassador simply has no external input surface.
	var unworn []string
	for _, r := range cell.Robes() {
		if len(p.Wearing(r)) == 0 {
			unworn = append(unworn, string(r))
		}
	}
	if len(unworn) > 0 {
		fmt.Printf("\nnobody wears %s, so this factory does not do those things\n",
			strings.Join(unworn, " or "))
	}
	fmt.Println("\nrobes carry no authority: they are a claim-policy input, and every cell")
	fmt.Println("above may do exactly what its enrolment scope says (--scope)")
	return nil
}
