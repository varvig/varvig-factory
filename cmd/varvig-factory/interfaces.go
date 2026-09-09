package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/varvig/varvig-factory/iface"
	"github.com/varvig/varvig-factory/reputation"
)

// The registry, from the command line.
//
// Publishing is how an interface hash comes to mean something: the effectful
// path refuses a hash the registry does not hold, so this is the step that makes
// a capability usable rather than merely named.
func cmdInterfaces(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		return interfacesList(args[1:])
	case "publish":
		return interfacesPublish(args[1:])
	case "show":
		return interfacesShow(args[1:])
	}
	return fmt.Errorf("interfaces: unknown subcommand %q (want list, publish or show)", args[0])
}

func interfacesList(args []string) error {
	f, err := parseFlags(args, nil, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	entries, listErr := iface.List(built.Factory)
	if listErr != nil {
		// Reported, not fatal: an unreadable entry is a name somebody meant to
		// register, and the rest of the registry is still the answer.
		fmt.Fprintf(os.Stderr, "warning: %v\n", listErr)
	}
	return emit(entries)
}

func interfacesPublish(args []string) error {
	f, err := parseFlags(args, []string{"alias", "schema"}, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	if err := requireAll(f.values, "alias", "schema"); err != nil {
		return err
	}
	raw, err := os.ReadFile(f.values["schema"])
	if err != nil {
		return fmt.Errorf("interfaces: reading %s: %w", f.values["schema"], err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("interfaces: %s is not a JSON object: %w", f.values["schema"], err)
	}
	hash, err := iface.Publish(built.Factory, f.values["alias"], schema)
	if err != nil {
		return err
	}
	return emit(iface.Entry{Alias: f.values["alias"], Hash: hash})
}

func interfacesShow(args []string) error {
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
	if hash == "" {
		return errors.New("interfaces: show needs --interface or --alias")
	}
	body, err := iface.Resolve(built.Factory, hash)
	if err != nil {
		return err
	}
	os.Stdout.Write(append(body, '\n'))
	return nil
}

// cmdReputation prints each cell's derived standing.
//
// It prints and stops there. Nothing in the loop reads this: a cell that
// consulted it to decide whether to attempt would be ordering work by a quality
// judgement, which is varvig's job and not a cell's (§10.1) — and it would
// compound, since a cell that attempts less has less record. The number is for
// whoever can see the whole picture.
func cmdReputation(args []string) error {
	f, err := parseFlags(args, nil, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	standings, err := reputation.Derive(built.Project)
	if err != nil {
		return err
	}
	if len(standings) == 0 {
		fmt.Println("no promoted tasks yet, so no cell has a record here")
		fmt.Println("standing is derived from what was promoted, and nothing has been")
		return nil
	}
	for _, s := range standings {
		fmt.Printf("%-20s %d of %d attempts promoted across %d task(s)",
			s.CellID, s.Promoted, s.Attempts, s.Tasks)
		if r := s.Rate(); r >= 0 {
			fmt.Printf("  (%.0f%%)", r*100)
		}
		fmt.Println()
	}
	return nil
}
