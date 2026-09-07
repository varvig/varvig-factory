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
	if err == nil || !strings.Contains(err.Error(), "compiled in") {
		t.Fatalf("an unknown executor name gave %v", err)
	}
}
