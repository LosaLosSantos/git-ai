package datastructure

// Set is a data structure that contains unique values. This data structure is not safe for
// concurrent accesses.
type Set[Value comparable] struct {
	values map[Value]struct{}
}

// NewSet returns a new ready-to-use set.
func NewSet[Value comparable]() *Set[Value] {
	return SetFromSlice[Value](nil)
}

// SetFromValues creates a new set that is populated with the provided values. The values will be
// deduplicated. This can be more efficient than manually creating and populating the set with
// values as the underlying data structure will already be preallocated.
func SetFromValues[Value comparable](values ...Value) *Set[Value] {
	return SetFromSlice(values)
}

// SetFromSlice creates a new set that is prepopulated with the values from the given slice. The
// slice values will be deduplicated. This can be more efficient than manually creating and
// populating the set with values as the underlying data structure will already be preallocated.
func SetFromSlice[Value comparable](slice []Value) *Set[Value] {
	values := make(map[Value]struct{}, len(slice))
	for _, value := range slice {
		values[value] = struct{}{}
	}

	return &Set[Value]{
		values: values,
	}
}

// Add adds the given value to the set. Returns `true` if the value was added, and `false` in case
// no change was made to the set.
func (s *Set[Value]) Add(v Value) bool {
	if _, exists := s.values[v]; !exists {
		s.values[v] = struct{}{}
		return true
	}

	return false
}

// Remove removes the given value from the set. Returns `true` if the value was removed, and `false`
// in case no change was made to the set.
func (s *Set[Value]) Remove(v Value) bool {
	if _, exists := s.values[v]; exists {
		delete(s.values, v)
		return true
	}

	return false
}

// HasValue determines whether the given value is contained in the set.
func (s *Set[Value]) HasValue(v Value) bool {
	_, exists := s.values[v]
	return exists
}

// Len returns the number of entries contained in the set.
func (s *Set[Value]) Len() int {
	return len(s.values)
}

// IsEmpty determines whether the set is empty.
func (s *Set[Value]) IsEmpty() bool {
	return s.Len() == 0
}

// Equal determines whether the set is equal to another set. Two sets are equal when they contain
// the exact same values.
func (s *Set[Value]) Equal(o *Set[Value]) bool {
	if s.Len() != o.Len() {
		return false
	}

	for _, value := range s.Values() {
		if !o.HasValue(value) {
			return false
		}
	}

	return true
}

// Values returns an array of all values that have been added to the set. The array is newly
// allocated and can be modified without affecting the set itself. The order of values is not
// guaranteed.
func (s *Set[Value]) Values() []Value {
	values := make([]Value, 0, len(s.values))
	for value := range s.values {
		values = append(values, value)
	}
	return values
}
