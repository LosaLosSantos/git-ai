package gitattributes

const (
	// Set indicates the attribute was specified, but without a state
	Set = "set"
	// Unset indicates the attribute was specified with a leading dash '-'
	Unset = "unset"
	// Unspecified indicates the attribute was not specified
	Unspecified = "unspecified"
)

// Attribute defines a single attribute. Name is the attribute and State can be
// either Set, Unset, or any other string value.
type Attribute struct {
	Name, State string
}

// Attributes is a set of attributes.
type Attributes []Attribute

// IsSet checks if the given attribute is set to a "set" value
func (attrs Attributes) IsSet(name string) bool {
	state, ok := attrs.StateFor(name)

	return ok && state == Set
}

// IsUnset checks if the given attribute is set to a "unset" value
func (attrs Attributes) IsUnset(name string) bool {
	state, ok := attrs.StateFor(name)

	return ok && state == Unset
}

// StateFor takes an attribute name and returns whether if was specified and if
// so what it's state is.
func (attrs Attributes) StateFor(name string) (string, bool) {
	for _, a := range attrs {
		if a.Name == name {
			return a.State, true
		}
	}

	return "", false
}
