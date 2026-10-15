package datastructure

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSet(t *testing.T) {
	t.Parallel()

	t.Run("new set is empty", func(t *testing.T) {
		s := NewSet[bool]()
		requireSet(t, s, []bool{})
	})

	t.Run("set from slice contains values", func(t *testing.T) {
		s := SetFromSlice([]int{1, 3, 5})
		requireSet(t, s, []int{1, 3, 5})
	})

	t.Run("set from slice deduplicates values", func(t *testing.T) {
		s := SetFromSlice([]int{1, 1, 3, 5, 5})
		requireSet(t, s, []int{1, 3, 5})
	})

	t.Run("set from values contains values", func(t *testing.T) {
		s := SetFromValues(1, 3, 5)
		requireSet(t, s, []int{1, 3, 5})
	})

	t.Run("set from values deduplicates values", func(t *testing.T) {
		s := SetFromValues(1, 1, 3, 5, 5)
		requireSet(t, s, []int{1, 3, 5})
	})

	t.Run("add value", func(t *testing.T) {
		s := NewSet[bool]()
		require.True(t, s.Add(true))

		requireSet(t, s, []bool{true})
		require.False(t, s.HasValue(false))
	})

	t.Run("remove value", func(t *testing.T) {
		s := NewSet[bool]()
		require.True(t, s.Add(true))
		require.True(t, s.Remove(true))

		requireSet(t, s, []bool{})
	})

	t.Run("adding multiple values", func(t *testing.T) {
		s := NewSet[int]()
		for i := 0; i < 5; i++ {
			require.True(t, s.Add(i))
		}

		requireSet(t, s, []int{0, 1, 2, 3, 4})
	})

	t.Run("adding and removing multiple values", func(t *testing.T) {
		s := NewSet[int]()
		for i := 0; i < 10; i++ {
			require.True(t, s.Add(i))
		}
		for i := 0; i < 5; i++ {
			require.True(t, s.Remove(i*2))
		}

		requireSet(t, s, []int{1, 3, 5, 7, 9})
	})

	t.Run("re-adding values", func(t *testing.T) {
		s := NewSet[int]()

		require.True(t, s.Add(1))
		require.False(t, s.Add(1))

		requireSet(t, s, []int{1})
	})

	t.Run("removing nonexistent value", func(t *testing.T) {
		s := NewSet[int]()

		require.True(t, s.Add(1))
		require.False(t, s.Remove(0))

		requireSet(t, s, []int{1})
	})
}

func TestSet_empty(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		a, b        *Set[int]
		expectEqual bool
	}{
		{
			desc:        "empty sets",
			a:           NewSet[int](),
			b:           NewSet[int](),
			expectEqual: true,
		},
		{
			desc:        "empty and non-empty set",
			a:           NewSet[int](),
			b:           SetFromValues(1),
			expectEqual: false,
		},
		{
			desc:        "disjunct sets",
			a:           SetFromValues(2),
			b:           SetFromValues(1),
			expectEqual: false,
		},
		{
			desc:        "partially disjunct sets",
			a:           SetFromValues(1, 2),
			b:           SetFromValues(1, 3),
			expectEqual: false,
		},
		{
			desc:        "subset",
			a:           SetFromValues(1),
			b:           SetFromValues(1, 3),
			expectEqual: false,
		},
		{
			desc:        "superset",
			a:           SetFromValues(1, 3),
			b:           SetFromValues(1),
			expectEqual: false,
		},
		{
			desc:        "same values",
			a:           SetFromValues(1, 2, 3),
			b:           SetFromValues(1, 2, 3),
			expectEqual: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expectEqual, tc.a.Equal(tc.b))
		})
	}
}

func requireSet[Value comparable](t *testing.T, s *Set[Value], expectedValues []Value) {
	t.Helper()

	require.Equal(t, len(expectedValues), s.Len())
	require.ElementsMatch(t, expectedValues, s.Values())
	require.Equal(t, len(expectedValues) == 0, s.IsEmpty())
	require.True(t, s.Equal(SetFromSlice(expectedValues)))
}
