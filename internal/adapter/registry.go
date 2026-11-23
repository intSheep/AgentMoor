package adapter

import (
	"fmt"
	"maps"
	"slices"
)

// Registry is constructed once at startup and then used for lookups.
type Registry struct {
	byID map[string]Adapter
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{byID: make(map[string]Adapter, len(adapters))}
	for _, a := range adapters {
		d := a.Descriptor()
		if d.ID == "" || d.Version == "" {
			return nil, fmt.Errorf("adapter ID and version are required")
		}
		if _, exists := r.byID[d.ID]; exists {
			return nil, fmt.Errorf("duplicate adapter %q", d.ID)
		}
		r.byID[d.ID] = a
	}
	return r, nil
}

func (r *Registry) Lookup(id string) (Adapter, error) {
	a, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", id)
	}
	return a, nil
}

func (r *Registry) All() []Adapter {
	result := make([]Adapter, 0, len(r.byID))
	for _, id := range slices.Sorted(maps.Keys(r.byID)) {
		result = append(result, r.byID[id])
	}
	return result
}
