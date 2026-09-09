package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/varvig/varvig-factory/authority"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/effect"
	"github.com/varvig/varvig-factory/iface"
	"github.com/varvig/varvig-factory/profile"
)

// The connector door.
//
// The protocol has always been repository state — a cell offers, a connector
// takes by compare-and-swap, executes, and reports; the cell settles. What was
// missing was any way to *speak* it from outside this module. Offer and settle
// were wired into the loop; awaiting, take and report had no caller but a test.
// So a vendor could not be added without importing the Go package and building
// a binary, which is the rebuild the protocol exists to avoid.
//
// These three verbs are that way in. A connector is any process that can run
// this binary: poll `connector awaiting`, `connector take` what it can serve,
// do the work, `connector report` the outcome. Output is JSON so the process
// driving it can be a shell script.
//
// # What a connector still cannot do
//
// It reports; it does not settle. Converting a hold into spend needs the lease,
// the lease lives in the coordination replica, and a connector is not trusted
// with it — a vendor that could write the lease could write its own payment. The
// cell picks up a report on its next pass and settles against the lease itself.
// That asymmetry is the design, not a missing feature, and it is why `take` and
// `report` need only the project replica.

func cmdConnector(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: varvig-factory connector <awaiting|take|report> [options]")
	}
	switch args[0] {
	case "awaiting":
		return connectorAwaiting(args[1:])
	case "take":
		return connectorTake(args[1:])
	case "report":
		return connectorReport(args[1:])
	}
	return fmt.Errorf("connector: unknown subcommand %q (want awaiting, take or report)", args[0])
}

// connectorAwaiting lists the offers a connector could take.
//
// The filter is the interface *hash*, not the alias: two factories may hold the
// same alias without agreeing who owns the name, and a connector that served an
// offer on the strength of a matching alias would be serving whatever that name
// happens to mean here. --alias is accepted as a convenience and resolved to a
// hash through the registry before anything is matched.
func connectorAwaiting(args []string) error {
	f, err := parseFlags(args, []string{"interface", "alias"}, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	hash, err := interfaceHash(built, f.values["interface"], f.values["alias"])
	if err != nil {
		return err
	}
	offers, err := effect.Awaiting(built.Project, hash)
	if err != nil {
		return err
	}
	return emit(offers)
}

// connectorTake claims one offer. Exactly one connector wins.
//
// The win is varvig's ordinary ref compare-and-swap and nothing else: no lock,
// no lease, no coordinator. A connector that loses is told so and must not
// execute — that refusal is the whole mechanism standing between two connectors
// and two identical orders.
func connectorTake(args []string) error {
	f, err := parseFlags(args, []string{"cell", "key", "connector", "deadline"}, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	if err := requireAll(f.values, "cell", "key", "connector"); err != nil {
		return err
	}
	var deadline int64
	if d := f.values["deadline"]; d != "" {
		secs, perr := time.ParseDuration(d)
		if perr != nil {
			return fmt.Errorf("connector: --deadline %q: %w", d, perr)
		}
		deadline = time.Now().Unix() + int64(secs.Seconds())
	}
	claim, err := effect.Take(built.Project, f.values["cell"], f.values["key"], f.values["connector"], time.Now().Unix(), deadline)
	if err != nil {
		return err
	}
	return emit(claim.Reservation)
}

// connectorReport records what happened at the far end.
//
// --happened is required and explicit, with no default, because the two answers
// are not near-misses of each other: one converts a hold into spend and the
// other gives it back. A flag whose absence meant either would make the most
// consequential field in the protocol the easiest one to leave out.
//
// A timeout is neither. A connector that did not hear back reports nothing and
// lets the reservation stand as pending, which is the honest record of an
// unknown outcome and the one state a higher principal has to resolve.
func connectorReport(args []string) error {
	f, err := parseFlags(args, []string{"cell", "key", "connector", "happened", "ref", "actual", "detail"}, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	if err := requireAll(f.values, "cell", "key", "connector", "happened"); err != nil {
		return err
	}
	var happened bool
	switch strings.ToLower(f.values["happened"]) {
	case "true", "yes":
		happened = true
	case "false", "no":
		happened = false
	default:
		return fmt.Errorf("connector: --happened must be true or false, got %q; "+
			"a report with an unclear outcome is a pending reservation, which is written by reporting nothing", f.values["happened"])
	}
	if happened && f.values["ref"] == "" {
		return errors.New("connector: reporting that it happened needs --ref, the far end's own identifier; " +
			"the next question about an unexpected invoice is which order it was, and the answer has to be in the record")
	}

	// A zero lease, deliberately. Reporting never touches one: Take hands back a
	// claim without a lease for the same reason, and effect.Settle refuses a
	// claim whose lease is missing rather than settling against an empty one. A
	// connector that carried a lease here would be a connector that could spend.
	claim, err := effect.LoadClaim(built.Project, f.values["cell"], f.values["key"], authority.Lease{}, "")
	if err != nil {
		return err
	}
	var actual cell.Money
	if a := f.values["actual"]; a != "" {
		actual, err = cell.ParseIn(claim.Reservation.Unit, a)
		if err != nil {
			return fmt.Errorf("connector: --actual: %w", err)
		}
	}
	reported, err := effect.Report(built.Project, claim, f.values["connector"], happened,
		f.values["ref"], actual, f.values["detail"], time.Now().Unix())
	if err != nil {
		return err
	}
	return emit(reported.Reservation)
}

// interfaceHash resolves whichever of --interface or --alias was given.
func interfaceHash(built profile.Built, hash, alias string) (string, error) {
	switch {
	case hash != "" && alias != "":
		return "", errors.New("connector: give --interface or --alias, not both; they can disagree and only one of them is the binding")
	case hash != "":
		if !iface.Known(built.Factory, hash) {
			return "", fmt.Errorf("connector: interface %s is not in this factory's registry", hash)
		}
		return hash, nil
	case alias != "":
		return iface.ByAlias(built.Factory, alias)
	}
	// No filter: every offer, whatever it needs.
	return "", nil
}

func requireAll(values map[string]string, names ...string) error {
	var missing []string
	for _, n := range names {
		if strings.TrimSpace(values[n]) == "" {
			missing = append(missing, "--"+n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("connector: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// emit writes JSON, because the process on the other end of this is a script.
func emit(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
