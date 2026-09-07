package profile

import (
	"strings"
	"testing"
)

func TestEffectConfigWiresCapabilitiesAndDefaultsToRefusing(t *testing.T) {
	iface := "1220a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	c := Mini("mini-a")
	c.Effects = EffectConfig{
		AuthorizedBy: "overseer-a",
		Capabilities: []EffectCapabilityConfig{{ID: "pcb-fabrication@1", Interface: iface}},
	}
	caps := c.Capabilities()
	if len(caps.Effects) != 1 || caps.Effects[0].Interface != iface {
		t.Fatalf("effects did not reach the capabilities object: %+v", caps.Effects)
	}
	if err := caps.Validate(); err != nil {
		t.Fatalf("the published capabilities are invalid: %v", err)
	}

	// An unnamed executor refuses rather than acting. A capability declared with
	// no integration must not silently become one that spends.
	ex, err := c.buildExecutors()
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 {
		t.Fatalf("built %d executors, want 1", len(ex))
	}

	// An alias with no interface hash is refused at startup, not at spend time.
	c.Effects.Capabilities = []EffectCapabilityConfig{{ID: "pcb-fabrication@1"}}
	if _, err := c.buildExecutors(); err == nil {
		t.Fatal("a capability with no interface hash was wired")
	}

	// An unknown executor name fails loudly rather than falling back to one that
	// acts — or to one that silently does not.
	c.Effects.Capabilities = []EffectCapabilityConfig{{ID: "x@1", Interface: iface, Executor: "acme-boards"}}
	_, err = c.buildExecutors()
	if err == nil || !strings.Contains(err.Error(), "connector peers") {
		t.Fatalf("an unknown executor name gave %v", err)
	}
}

func TestConnectorServedCapabilitiesGetNoInProcessExecutor(t *testing.T) {
	iface := "1220a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	c := Mini("mini-a")
	c.Effects = EffectConfig{
		AuthorizedBy: "overseer-a",
		Capabilities: []EffectCapabilityConfig{
			{ID: "pcb-fabrication@1", Interface: iface, Executor: "connector"},
			{ID: "shipping@1", Interface: iface, Executor: "refusing"},
		},
	}

	ex, err := c.buildExecutors()
	if err != nil {
		t.Fatal(err)
	}
	// Only the refusing one is in-process; the connector-served capability has
	// nothing here by design, because it is performed in another process.
	if len(ex) != 1 {
		t.Fatalf("built %d in-process executors, want 1", len(ex))
	}
	served := c.connectorCapabilities()
	if !served[iface] {
		t.Fatalf("the connector-served capability was not marked: %v", served)
	}

	// Both still reach the published capabilities: what a cell can perform is a
	// static fact whether or not it performs it in-process.
	if len(c.Capabilities().Effects) != 2 {
		t.Fatalf("effects = %+v, want both declared", c.Capabilities().Effects)
	}

	// A cell with no connector-served capability reports none, rather than an
	// empty map that reads as "configured, but nothing in it".
	c.Effects.Capabilities = []EffectCapabilityConfig{{ID: "shipping@1", Interface: iface}}
	if c.connectorCapabilities() != nil {
		t.Fatal("a cell with no connector capabilities reported a set")
	}
}
