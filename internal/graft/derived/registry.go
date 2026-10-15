package derived

import (
	"fmt"
	"slices"
)

// Registry lists the derived kinds of this build by name. The names are the store
// kinds and the values of the [graft.derived] kinds setting, so a selection is
// validated against the registry before any work starts.
type Registry struct {
	names []string
	known map[string]struct{}
}

// NewRegistry returns a registry of the given kinds. Names must be unique and match
// the store's kind syntax.
func NewRegistry(names ...string) (*Registry, error) {
	r := &Registry{names: make([]string, 0, len(names)), known: make(map[string]struct{}, len(names))}

	for _, name := range names {
		if !kindRegexp.MatchString(name) {
			return nil, fmt.Errorf("invalid derived kind %q", name)
		}
		if _, ok := r.known[name]; ok {
			return nil, fmt.Errorf("duplicate derived kind %q", name)
		}

		r.known[name] = struct{}{}
		r.names = append(r.names, name)
	}

	return r, nil
}

// DefaultRegistry returns the registry of the kinds that this build computes.
func DefaultRegistry() *Registry {
	r, err := NewRegistry(kindLastMod, kindLastModFirstParent, kindBlameOrigin, kindBlame)
	if err != nil {
		// The kinds are constants that are valid by construction.
		panic(err)
	}
	return r
}

// Names returns the registered kinds in registration order.
func (r *Registry) Names() []string {
	return slices.Clone(r.names)
}

// Select returns the kinds named by selection, in registration order. An empty
// selection selects every kind, which is the default of the kinds setting. Unknown
// names are an error, so that a misspelt entry is never silently ignored.
func (r *Registry) Select(selection []string) ([]string, error) {
	if len(selection) == 0 {
		return r.Names(), nil
	}

	wanted := make(map[string]struct{}, len(selection))
	for _, name := range selection {
		if _, ok := r.known[name]; !ok {
			return nil, fmt.Errorf("unknown derived kind %q", name)
		}
		wanted[name] = struct{}{}
	}

	selected := make([]string, 0, len(wanted))
	for _, name := range r.names {
		if _, ok := wanted[name]; ok {
			selected = append(selected, name)
		}
	}

	return selected, nil
}
