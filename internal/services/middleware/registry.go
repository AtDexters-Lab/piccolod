package middleware

import (
	"fmt"
	"sync"
)

// Canonical middleware names that the conditional-canonical gate
// (canonicalApplies) needs to recognize. Defined here (in middleware/) so
// canonicalApplies can switch on them without importing the builtin
// subpackage. Other names (forwarded_scrub, conn_metrics, etc.) are
// always-canonical and live in builtin/builtin.go.
const (
	NameConnectionAuth    = "connection_auth"
	NameConnectionAuthUDP = "connection_auth_udp"
	NamePathAuth          = "path_auth"
)

// Registry holds named middleware factories. Factories register at package init time
// (typically) and are looked up at endpoint-build time to compose per-listener chains.
//
// Build-time composition rule:
//   - Canonical entries (registered as canonical) run first in their layer's chain
//     in their canonical order.
//   - Operator-listed entries (from listener.Middleware[]) append to the end of
//     the canonical chain in declaration order.
//   - No reordering syntax (no position:); no removal of canonical entries.
//   - Unknown name → Build returns error; reconcile rejects listener fail-closed.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
	// canonical orders the names that compose the canonical chain for each layer.
	// Order within a slice IS the canonical order of execution.
	canonical map[Layer][]string
}

type registryEntry struct {
	layers    []Layer
	factory   Factory
	canonical bool // true when registered via RegisterCanonical; rejects operator-listing
}

// NewRegistry constructs an empty registry. Built-in factories register themselves
// against this instance via Register/RegisterCanonical at services package init.
func NewRegistry() *Registry {
	return &Registry{
		entries:   map[string]registryEntry{},
		canonical: map[Layer][]string{},
	}
}

// Register adds a middleware factory under name, valid for the given layers.
// Operator-listable middlewares register here. Built-ins that should always run register
// via RegisterCanonical.
//
// Panics on:
//   - empty name, empty layers, or nil factory (programming error at init time)
//   - duplicate layers in the layers slice (sibling-shape latent bug)
//   - re-registration of an existing name (fail-fast against accidental double-reg
//     across init() ordering or two packages registering the same name; tests that
//     need to override should construct a fresh Registry)
func (r *Registry) Register(name string, layers []Layer, factory Factory) {
	r.validateRegistration("Register", name, layers, factory)
	seen := map[Layer]bool{}
	for _, l := range layers {
		if seen[l] {
			panic(fmt.Sprintf("middleware.Register: duplicate layer %s in %q", l, name))
		}
		seen[l] = true
	}
	r.insertEntry("Register", name, registryEntry{layers: layers, factory: factory, canonical: false})
}

// RegisterCanonical registers a factory AND appends its name to the canonical
// chain for the given layer. Canonical entries always run first in their layer's
// chain, in registration order.
//
// Canonical entries are NEVER operator-listable — listing a canonical name in a
// listener's Middleware[] field is rejected at Build time.
//
// To register the same factory for additional non-canonical layers (e.g.,
// hypothetically: a primarily-canonical-on-L4 factory that's also operator-listable
// on L4UDP), use a SEPARATE name for the non-canonical registration, or design the
// factory's two roles as two distinct Register calls under different names. Mixing
// canonical and non-canonical responsibilities under one name is rejected to
// preserve the canonical-vs-operator distinction at the registry layer.
//
// Panics on:
//   - empty name or nil factory
//   - re-registration (canonical or operator) of an existing name (fail-fast)
func (r *Registry) RegisterCanonical(name string, layer Layer, factory Factory) {
	r.validateRegistration("RegisterCanonical", name, []Layer{layer}, factory)
	r.insertEntry("RegisterCanonical", name, registryEntry{layers: []Layer{layer}, factory: factory, canonical: true})
	r.mu.Lock()
	r.canonical[layer] = append(r.canonical[layer], name)
	r.mu.Unlock()
}

// validateRegistration enforces shared init-time argument checks.
func (r *Registry) validateRegistration(caller, name string, layers []Layer, factory Factory) {
	if name == "" {
		panic(fmt.Sprintf("middleware.%s: empty name", caller))
	}
	if len(layers) == 0 {
		panic(fmt.Sprintf("middleware.%s: empty layers", caller))
	}
	if factory == nil {
		panic(fmt.Sprintf("middleware.%s: nil factory", caller))
	}
}

// insertEntry takes the write lock, rejects duplicates, and stores the entry.
func (r *Registry) insertEntry(caller, name string, entry registryEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[name]; exists {
		panic(fmt.Sprintf("middleware.%s: %q already registered", caller, name))
	}
	r.entries[name] = entry
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

// Build composes the full BuildResult (all four layers). Useful when a caller
// needs every chain at once (e.g., test harness). Production listener callers
// build per-layer via BuildL4 / BuildL4UDP / BuildL7 / BuildL7Response so each
// layer's deps stay scoped to that layer's factories.
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
// Reconcile callers treat any Build error as fail-closed: listener is rejected
// with a config_error health badge rather than starting with a partial chain.
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

// BuildL4 composes the L4 (TCP-conn) chain only. Lets callers scope deps to
// the L4 factories without pulling in L7 dep requirements.
func (r *Registry) BuildL4(spec BuildSpec) ([]L4Middleware, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return buildLayer[L4Middleware](r, spec, LayerL4)
}

// BuildL4UDP composes the L4UDP (datagram) chain only.
func (r *Registry) BuildL4UDP(spec BuildSpec) ([]L4UDPMiddleware, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return buildLayer[L4UDPMiddleware](r, spec, LayerL4UDP)
}

// BuildL7 composes the L7 (HTTP request-side) chain only.
func (r *Registry) BuildL7(spec BuildSpec) ([]L7Middleware, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return buildLayer[L7Middleware](r, spec, LayerL7)
}

// BuildL7Response composes the L7Response (HTTP response-side) chain only.
func (r *Registry) BuildL7Response(spec BuildSpec) ([]ResponseModifier, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return buildLayer[ResponseModifier](r, spec, LayerL7Response)
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
		// Reject canonical names listed by operator. Canonical entries are composed via
		// dedicated mechanisms (always-on + conditional via typed listener fields like
		// ConnectionAuth/Auth). Listing them in Middleware[] is a config error.
		if ent.canonical {
			return nil, fmt.Errorf("middleware %q: canonical entry not operator-listable (composed automatically; remove from Middleware[])", entry.Name)
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
// If a third gate ever lands here, refactor to a Gates map[string]bool on BuildSpec
// rather than continuing to grow the switch + Has* fields.
func canonicalApplies(name string, spec BuildSpec) bool {
	switch name {
	case NameConnectionAuth, NameConnectionAuthUDP:
		return spec.HasConnectionAuth
	case NamePathAuth:
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
	mw, err := ent.factory(params, spec.Endpoint, spec.Deps, layer)
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
