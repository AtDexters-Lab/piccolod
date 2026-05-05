package middleware

import (
	"fmt"
	"sync"
)

// Registry holds named middleware factories. Factories register at package init time
// (typically) and are looked up at endpoint-build time to compose per-listener chains.
//
// Build-time composition rule per plan D7:
//   - Canonical entries (registered as canonical) run first in their layer's chain
//     in their canonical order.
//   - Operator-listed entries (from listener.Middleware[]) append to the end of
//     the canonical chain in declaration order.
//   - No reordering syntax (no position:); no removal of canonical entries.
//   - Unknown name → Build returns error; reconcile rejects listener fail-closed
//     per S5.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
	// canonical orders the names that compose the canonical chain for each layer.
	// Order within a slice IS the canonical order of execution.
	canonical map[Layer][]string
}

type registryEntry struct {
	layers  []Layer
	factory Factory
}

// NewRegistry constructs an empty registry. Built-in factories register themselves
// against this instance via Register/RegisterCanonical at services package init.
func NewRegistry() *Registry {
	return &Registry{
		entries:   map[string]registryEntry{},
		canonical: map[Layer][]string{},
	}
}

// Register adds (or replaces) a middleware factory under name, valid for the given layers.
// Operator-listable middlewares register here. Built-ins that should always run register
// via RegisterCanonical.
func (r *Registry) Register(name string, layers []Layer, factory Factory) {
	if name == "" {
		panic("middleware.Register: empty name")
	}
	if len(layers) == 0 {
		panic("middleware.Register: empty layers")
	}
	if factory == nil {
		panic("middleware.Register: nil factory")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = registryEntry{layers: layers, factory: factory}
}

// RegisterCanonical registers a factory AND appends its name to the canonical
// chain for the given layer. Canonical entries always run first in their layer's
// chain, in registration order.
//
// A factory may register canonically in multiple layers (e.g., hint_consumer_l4
// in LayerL4 only, but ip_allowlist could register in both L4 and L4UDP — that
// case uses Register, not RegisterCanonical, since ip_allowlist is operator-listable).
func (r *Registry) RegisterCanonical(name string, layer Layer, factory Factory) {
	if name == "" {
		panic("middleware.RegisterCanonical: empty name")
	}
	if factory == nil {
		panic("middleware.RegisterCanonical: nil factory")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = registryEntry{layers: []Layer{layer}, factory: factory}
	r.canonical[layer] = append(r.canonical[layer], name)
}

// BuildSpec describes which middlewares Build should compose.
//
// The services package constructs a BuildSpec by:
//   - Setting Endpoint from a ServiceEndpoint.
//   - Listing operator middlewares (from listener.Middleware[]) in OperatorEntries.
//   - Including ConnectionAuth/Auth presence flags so canonical middlewares that are
//     conditionally composed (e.g., connection_auth only when ConnectionAuth field is
//     non-nil) can be included or skipped.
//
// Build does not consume listener.Middleware[] directly — it stays opaque to the
// shape of that field; the services package translates it into operator entries.
type BuildSpec struct {
	Endpoint          EndpointInfo
	OperatorEntries   []OperatorEntry
	HasConnectionAuth bool // gate for conditionally-canonical connection_auth
	HasAuth           bool // gate for conditionally-canonical path_auth
	Deps              RegistryDeps
}

// OperatorEntry is one entry from a listener's Middleware[] field, translated into
// the middleware package's input shape (the package itself does not import the api
// types defining AppListener).
type OperatorEntry struct {
	Name   string
	Params map[string]any
}

// BuildResult holds the composed chains for each layer. A chain is a slice of
// middleware values, ordered for execution (head of slice runs first).
type BuildResult struct {
	L4         []L4Middleware
	L4UDP      []L4UDPMiddleware
	L7         []L7Middleware
	L7Response []ResponseModifier
}

// Build composes the per-endpoint chains.
//
// Algorithm:
//  1. For each layer, walk the canonical names in registration order. Skip names
//     for conditionally-canonical entries when the gate is false (e.g., skip
//     connection_auth when spec.HasConnectionAuth is false).
//  2. Append operator entries in declared order. Each operator entry's factory
//     must be registered for the layer being composed.
//  3. For each chosen entry, invoke the factory with params + endpoint + deps,
//     type-assert the return to the layer's middleware type, append to the chain.
//
// Returns error on:
//   - Operator entry references unknown middleware name.
//   - Operator entry references a name registered only as canonical-internal
//     (today: enforced via the conditionally-canonical gate, not a separate flag).
//   - Factory returns error.
//   - Factory return is not the expected type for the layer.
//
// Reconcile callers treat any Build error as fail-closed per S5: listener is
// rejected with a config_error health badge.
//
// For step 1 of the plan landing: with no factories registered yet, Build returns
// an empty BuildResult successfully. Real composition kicks in when step 5 lands
// the built-in factories.
func (r *Registry) Build(spec BuildSpec) (BuildResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result BuildResult
	var err error

	if result.L4, err = buildLayer[L4Middleware](r, spec, LayerL4); err != nil {
		return BuildResult{}, fmt.Errorf("L4 chain: %w", err)
	}
	if result.L4UDP, err = buildLayer[L4UDPMiddleware](r, spec, LayerL4UDP); err != nil {
		return BuildResult{}, fmt.Errorf("L4UDP chain: %w", err)
	}
	if result.L7, err = buildLayer[L7Middleware](r, spec, LayerL7); err != nil {
		return BuildResult{}, fmt.Errorf("L7 chain: %w", err)
	}
	if result.L7Response, err = buildLayer[ResponseModifier](r, spec, LayerL7Response); err != nil {
		return BuildResult{}, fmt.Errorf("L7Response chain: %w", err)
	}

	return result, nil
}

// buildLayer composes one layer's chain. Generic over the middleware type so the
// layer-specific type assertion lives in one place.
//
// Caller holds r.mu.RLock().
func buildLayer[M any](r *Registry, spec BuildSpec, layer Layer) ([]M, error) {
	var chain []M

	// Canonical entries first.
	for _, name := range r.canonical[layer] {
		if !canonicalApplies(name, spec) {
			continue
		}
		mw, err := r.invokeFactory(name, layer, nil, spec)
		if err != nil {
			return nil, err
		}
		typed, ok := mw.(M)
		if !ok {
			return nil, fmt.Errorf("middleware %q: factory returned %T, expected %s", name, mw, layer)
		}
		chain = append(chain, typed)
	}

	// Operator entries appended in declaration order.
	for _, entry := range spec.OperatorEntries {
		ent, ok := r.entries[entry.Name]
		if !ok {
			return nil, fmt.Errorf("middleware %q: not registered", entry.Name)
		}
		// Skip operator entries that aren't valid for this layer (e.g., an L7-only
		// middleware listed on a UDP listener — silently skipped at this layer,
		// would compose into L7 chain if the listener had one).
		if !layerInList(layer, ent.layers) {
			continue
		}
		mw, err := r.invokeFactory(entry.Name, layer, entry.Params, spec)
		if err != nil {
			return nil, err
		}
		typed, ok := mw.(M)
		if !ok {
			return nil, fmt.Errorf("middleware %q: factory returned %T, expected %s", entry.Name, mw, layer)
		}
		chain = append(chain, typed)
	}

	return chain, nil
}

// canonicalApplies decides whether a canonical entry should run for this spec.
//
// Conditionally-canonical entries are included only when their gate is set in the
// spec. Unconditional canonical entries always apply.
//
// Step 1 has no canonical registrations; the gate logic stubs are in place for
// step 5 when built-ins land.
func canonicalApplies(name string, spec BuildSpec) bool {
	switch name {
	case "connection_auth":
		return spec.HasConnectionAuth
	case "path_auth":
		return spec.HasAuth
	default:
		return true
	}
}

func (r *Registry) invokeFactory(name string, layer Layer, params map[string]any, spec BuildSpec) (any, error) {
	ent, ok := r.entries[name]
	if !ok {
		return nil, fmt.Errorf("middleware %q: not registered (canonical chain references missing entry)", name)
	}
	if !layerInList(layer, ent.layers) {
		return nil, fmt.Errorf("middleware %q: not registered for layer %s", name, layer)
	}
	mw, err := ent.factory(params, spec.Endpoint, spec.Deps)
	if err != nil {
		return nil, fmt.Errorf("middleware %q factory: %w", name, err)
	}
	return mw, nil
}

func layerInList(layer Layer, list []Layer) bool {
	for _, l := range list {
		if l == layer {
			return true
		}
	}
	return false
}
