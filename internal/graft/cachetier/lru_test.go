package cachetier

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLRU(t *testing.T) {
	t.Parallel()

	sizeOf := func(v int64) int64 { return v }

	t.Run("evicts least recently used first", func(t *testing.T) {
		evicted := 0
		c := newLRU[string, int64](30, sizeOf, func() { evicted++ })

		c.add("a", 10)
		c.add("b", 10)
		c.add("c", 10)

		// Touch a so that b becomes the least recently used entry.
		_, ok := c.get("a")
		require.True(t, ok)

		c.add("d", 10)

		_, ok = c.get("b")
		require.False(t, ok, "b should have been evicted")
		for _, key := range []string{"a", "c", "d"} {
			_, ok := c.get(key)
			require.True(t, ok, "%s should be retained", key)
		}
		require.Equal(t, 1, evicted)

		entries, bytes := c.usage()
		require.Equal(t, 3, entries)
		require.Equal(t, int64(30), bytes)
	})

	t.Run("bounds the total size", func(t *testing.T) {
		c := newLRU[int, int64](100, sizeOf, nil)
		for i := 0; i < 50; i++ {
			c.add(i, 7)
		}
		_, bytes := c.usage()
		require.LessOrEqual(t, bytes, int64(100))
	})

	t.Run("does not store values larger than the budget", func(t *testing.T) {
		c := newLRU[string, int64](10, sizeOf, nil)
		c.add("small", 5)
		c.add("huge", 11)

		_, ok := c.get("huge")
		require.False(t, ok)
		_, ok = c.get("small")
		require.True(t, ok, "an oversized add must not evict existing entries")
	})

	t.Run("replacing a key keeps accounting correct", func(t *testing.T) {
		c := newLRU[string, int64](100, sizeOf, nil)
		c.add("k", 40)
		c.add("k", 25)

		entries, bytes := c.usage()
		require.Equal(t, 1, entries)
		require.Equal(t, int64(25), bytes)
	})
}
