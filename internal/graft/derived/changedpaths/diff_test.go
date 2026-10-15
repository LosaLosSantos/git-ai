package changedpaths

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChangedFiles(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		build func(f *fakeObjects) (oldTree, newTree string)
		want  []string
	}{
		{
			name: "identical trees",
			build: func(f *fakeObjects) (string, string) {
				tree := f.tree(blob("a", "1"))
				return tree, tree
			},
		},
		{
			name: "added file in a subdirectory",
			build: func(f *fakeObjects) (string, string) {
				old := f.tree(blob("a", "1"))
				sub := f.tree(blob("e", "2"))
				return old, f.tree(blob("a", "1"), subtree("d", sub))
			},
			want: []string{"d/e"},
		},
		{
			name: "modified content",
			build: func(f *fakeObjects) (string, string) {
				old := f.tree(blob("a", "1"), subtree("d", f.tree(blob("e", "2"))))
				return old, f.tree(blob("a", "1"), subtree("d", f.tree(blob("e", "3"))))
			},
			want: []string{"d/e"},
		},
		{
			name: "mode change only",
			build: func(f *fakeObjects) (string, string) {
				old := f.tree(blob("x", "1"))
				return old, f.tree(Entry{Name: "x", Mode: 0o100755, OID: "1"})
			},
			want: []string{"x"},
		},
		{
			name: "removed directory",
			build: func(f *fakeObjects) (string, string) {
				old := f.tree(subtree("d", f.tree(blob("e", "2"), blob("f", "3"))))
				return old, f.tree()
			},
			want: []string{"d/e", "d/f"},
		},
		{
			name: "file replaced by a directory",
			build: func(f *fakeObjects) (string, string) {
				old := f.tree(blob("x", "1"))
				return old, f.tree(subtree("x", f.tree(blob("y", "2"))))
			},
			want: []string{"x", "x/y"},
		},
		{
			name: "empty directory added",
			build: func(f *fakeObjects) (string, string) {
				return f.tree(), f.tree(subtree("d", f.tree()))
			},
		},
		{
			name: "root commit compares with the empty tree",
			build: func(f *fakeObjects) (string, string) {
				return "", f.tree(blob("a", "1"), subtree("d", f.tree(blob("e", "2"))))
			},
			want: []string{"a", "d/e"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeObjects()
			oldTree, newTree := tc.build(f)
			got, overflow, err := changedFiles(context.Background(), f, oldTree, newTree, "", 0)
			require.NoError(t, err)
			require.False(t, overflow)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestChangedFiles_Filter(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	newTree := f.tree(subtree("a", f.tree(blob("b", "1"))), blob("c", "2"))

	for filter, want := range map[string][]string{
		"":      {"a/b", "c"},
		"a":     {"a/b"},
		"a/b":   {"a/b"},
		"c":     {"c"},
		"a/b/c": nil, // a/b is an ancestor of the filter, not below it
		"x":     nil,
	} {
		got, _, err := changedFiles(context.Background(), f, "", newTree, filter, 0)
		require.NoError(t, err)
		require.Equal(t, want, got, "filter %q", filter)
	}
}

func TestChangedFiles_Limit(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	newTree := f.tree(blob("a", "1"), blob("b", "2"), blob("c", "3"))

	got, overflow, err := changedFiles(context.Background(), f, "", newTree, "", 2)
	require.NoError(t, err)
	require.True(t, overflow)
	require.Nil(t, got)

	got, overflow, err = changedFiles(context.Background(), f, "", newTree, "", 3)
	require.NoError(t, err)
	require.False(t, overflow)
	require.Equal(t, []string{"a", "b", "c"}, got)
}

func TestChangedFiles_Cancelled(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	newTree := f.tree(blob("a", "1"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := changedFiles(ctx, f, "", newTree, "", 0)
	require.ErrorIs(t, err, context.Canceled)
}
