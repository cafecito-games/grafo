package calleffect_test

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/parser/calleffect"
)

func TestRegistryCanonicalizesEffectOrderAndOwnsSelectors(t *testing.T) {
	roles := map[string]calleffect.Selector{
		calleffect.RoleEvent:   {Argument: 0},
		calleffect.RoleHandler: {Argument: 1},
	}
	registry, err := calleffect.New([]calleffect.Adapter{{
		Language: "gdscript", Symbol: "Signals.route", Line: 1,
		Effects: []calleffect.Effect{
			{Kind: calleffect.EventSubscribe, Roles: roles, Line: 5},
			{Kind: calleffect.EventPublish, Roles: map[string]calleffect.Selector{calleffect.RoleEvent: {Argument: 0}}, Line: 3},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	roles[calleffect.RoleEvent] = calleffect.Selector{Argument: 9}
	effects := registry.Lookup("gdscript", "Signals.route")
	if len(effects) != 2 || effects[0].Kind != calleffect.EventPublish || effects[1].Kind != calleffect.EventSubscribe {
		t.Fatalf("canonical effects = %#v", effects)
	}
	if effects[1].Roles[calleffect.RoleEvent].Argument != 0 {
		t.Fatalf("registry retained caller-owned selector map: %#v", effects[1].Roles)
	}
}
