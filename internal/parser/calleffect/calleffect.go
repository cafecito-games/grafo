// Package calleffect defines the closed, language-neutral vocabulary for
// repository-declared call effects. Language frontends resolve syntax and
// domain projectors remain responsible for emitting valid graph facts.
package calleffect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type Kind string

const (
	EventPublish        Kind = "event.publish"
	EventSubscribe      Kind = "event.subscribe"
	EventUnsubscribe    Kind = "event.unsubscribe"
	EventConnectionTest Kind = "event.connection_test"
	HTTPRequest         Kind = "http.request"
)

const (
	RoleEvent   = "event"
	RoleHandler = "handler"
	RoleMethod  = "method"
	RoleURL     = "url"
)

const (
	PropertySource = "adapter_source"
	PropertySymbol = "adapter_symbol"
)

type Selector struct {
	Argument int
	Line     int
	Column   int
}

type Effect struct {
	Kind   Kind
	Roles  map[string]Selector
	Line   int
	Column int
}

type Adapter struct {
	Language string
	Symbol   string
	Effects  []Effect
	Line     int
	Column   int
	Legacy   bool
}

type Registry struct {
	adapters []Adapter
	byCall   map[string][]Effect
	lines    map[string]int
}

func New(adapters []Adapter) (Registry, error) {
	result := Registry{adapters: make([]Adapter, 0, len(adapters)), byCall: map[string][]Effect{}, lines: map[string]int{}}
	seenEffects := map[string]int{}
	for _, adapter := range adapters {
		if len(adapter.Effects) == 0 {
			return Registry{}, fmt.Errorf("line %d: adapter effects must be a non-empty sequence", adapter.Line)
		}
		identity := adapter.Language + "\x00" + adapter.Symbol
		if previous, ok := result.lines[identity]; ok {
			return Registry{}, fmt.Errorf("line %d: duplicate adapter symbol %q (first declared on line %d)", adapter.Line, adapter.Symbol, previous)
		}
		result.lines[identity] = adapter.Line
		cloned := adapter
		cloned.Effects = make([]Effect, 0, len(adapter.Effects))
		for _, effect := range adapter.Effects {
			if err := ValidateEffect(effect); err != nil {
				return Registry{}, err
			}
			key := identity + "\x00" + string(effect.Kind)
			if previous, ok := seenEffects[key]; ok {
				return Registry{}, fmt.Errorf("line %d: duplicate effect %q for adapter %q (first declared on line %d)",
					effect.Line, effect.Kind, adapter.Symbol, previous)
			}
			seenEffects[key] = effect.Line
			clonedEffect := effect
			clonedEffect.Roles = make(map[string]Selector, len(effect.Roles))
			for role, selector := range effect.Roles {
				clonedEffect.Roles[role] = selector
			}
			cloned.Effects = append(cloned.Effects, clonedEffect)
		}
		sort.Slice(cloned.Effects, func(i, j int) bool { return cloned.Effects[i].Kind < cloned.Effects[j].Kind })
		result.adapters = append(result.adapters, cloned)
	}
	sort.Slice(result.adapters, func(i, j int) bool {
		if result.adapters[i].Language != result.adapters[j].Language {
			return result.adapters[i].Language < result.adapters[j].Language
		}
		return result.adapters[i].Symbol < result.adapters[j].Symbol
	})
	for _, adapter := range result.adapters {
		identity := adapter.Language + "\x00" + adapter.Symbol
		result.byCall[identity] = append(result.byCall[identity], adapter.Effects...)
	}
	return result, nil
}

func ValidateEffect(effect Effect) error {
	var required []string
	switch effect.Kind {
	case EventPublish:
		required = []string{RoleEvent}
	case EventSubscribe, EventUnsubscribe, EventConnectionTest:
		required = []string{RoleEvent, RoleHandler}
	case HTTPRequest:
		required = []string{RoleMethod, RoleURL}
	default:
		return fmt.Errorf("line %d: unsupported adapter effect %q", effect.Line, effect.Kind)
	}
	allowed := map[string]bool{}
	seenArguments := map[int]string{}
	for _, role := range required {
		allowed[role] = true
		selector, ok := effect.Roles[role]
		if !ok {
			return fmt.Errorf("line %d: adapter effect %q requires role %q", effect.Line, effect.Kind, role)
		}
		if selector.Argument < 0 {
			return fmt.Errorf("line %d: adapter role %q argument must be non-negative", selector.Line, role)
		}
		if previous, exists := seenArguments[selector.Argument]; exists {
			return fmt.Errorf("line %d: adapter effect %q roles %q and %q must use distinct argument indexes",
				effect.Line, effect.Kind, previous, role)
		}
		seenArguments[selector.Argument] = role
	}
	for role := range effect.Roles {
		if !allowed[role] {
			return fmt.Errorf("line %d: adapter effect %q has unknown role %q", effect.Line, effect.Kind, role)
		}
	}
	return nil
}

func (r Registry) Lookup(language, symbol string) []Effect {
	stored := r.byCall[language+"\x00"+symbol]
	result := make([]Effect, 0, len(stored))
	for _, effect := range stored {
		cloned := effect
		cloned.Roles = make(map[string]Selector, len(effect.Roles))
		for role, selector := range effect.Roles {
			cloned.Roles[role] = selector
		}
		result = append(result, cloned)
	}
	return result
}

func (r Registry) SemanticKey() string {
	type canonicalSelector struct {
		Role     string `json:"role"`
		Argument int    `json:"argument"`
	}
	type canonicalEffect struct {
		Kind      Kind                `json:"kind"`
		Selectors []canonicalSelector `json:"selectors"`
	}
	type canonicalAdapter struct {
		Language string            `json:"language"`
		Symbol   string            `json:"symbol"`
		Effects  []canonicalEffect `json:"effects"`
	}
	canonical := make([]canonicalAdapter, 0, len(r.adapters))
	for _, adapter := range r.adapters {
		item := canonicalAdapter{Language: adapter.Language, Symbol: adapter.Symbol,
			Effects: make([]canonicalEffect, 0, len(adapter.Effects))}
		for _, effect := range adapter.Effects {
			entry := canonicalEffect{Kind: effect.Kind}
			for role, selector := range effect.Roles {
				entry.Selectors = append(entry.Selectors, canonicalSelector{Role: role, Argument: selector.Argument})
			}
			sort.Slice(entry.Selectors, func(i, j int) bool { return entry.Selectors[i].Role < entry.Selectors[j].Role })
			item.Effects = append(item.Effects, entry)
		}
		sort.Slice(item.Effects, func(i, j int) bool { return item.Effects[i].Kind < item.Effects[j].Kind })
		canonical = append(canonical, item)
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Language != canonical[j].Language {
			return canonical[i].Language < canonical[j].Language
		}
		return canonical[i].Symbol < canonical[j].Symbol
	})
	encoded, _ := json.Marshal(canonical)
	digest := sha256.Sum256(encoded)
	return "call-effect-v1:" + hex.EncodeToString(digest[:])
}
