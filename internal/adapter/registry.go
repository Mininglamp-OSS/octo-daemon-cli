package adapter

import "fmt"

// Registry maps a runtime_kind string to its adapter. The daemon builds one at
// startup, registers each supported adapter, and resolves per-command via Get.
// It is not safe for concurrent registration; register all adapters during
// single-threaded startup, then only Get/All afterwards.
type Registry struct {
	adapters map[string]RuntimeAdapter
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]RuntimeAdapter)}
}

// Register adds an adapter under its Kind(). It returns an error on duplicate
// kinds so a wiring mistake fails loudly at startup.
func (r *Registry) Register(a RuntimeAdapter) error {
	kind := a.Kind()
	if kind == "" {
		return fmt.Errorf("adapter has empty kind")
	}
	if _, exists := r.adapters[kind]; exists {
		return fmt.Errorf("adapter already registered for kind %q", kind)
	}
	r.adapters[kind] = a
	return nil
}

// Get resolves the adapter for a runtime_kind. An empty kind is normalized to
// "openclaw" to stay backward-compatible with fleet commands that predate the
// runtime_kind field.
func (r *Registry) Get(kind string) (RuntimeAdapter, error) {
	a, ok := r.adapters[normalizeRuntimeKind(kind)]
	if !ok {
		return nil, fmt.Errorf("no adapter registered for kind %q", kind)
	}
	return a, nil
}

// All returns every registered adapter (order unspecified).
func (r *Registry) All() []RuntimeAdapter {
	out := make([]RuntimeAdapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		out = append(out, a)
	}
	return out
}

// Kinds returns the set of registered runtime_kind strings. The daemon uses it
// as the authoritative allowlist when filtering provider lists pushed by fleet.
func (r *Registry) Kinds() map[string]struct{} {
	out := make(map[string]struct{}, len(r.adapters))
	for k := range r.adapters {
		out[k] = struct{}{}
	}
	return out
}

// normalizeRuntimeKind defaults an empty kind to openclaw.
func normalizeRuntimeKind(kind string) string {
	if kind == "" {
		return KindOpenclaw
	}
	return kind
}
