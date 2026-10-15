package reftree

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTree(t *testing.T) {
	t.Parallel()

	t.Run("InsertReference requires fully-qualified reference", func(t *testing.T) {
		t.Parallel()

		root := New()

		require.Equal(t,
			root.InsertReference("non-refs/prefixed"),
			errors.New("expected a fully qualified reference"),
		)
	})

	t.Run("InsertNode", func(t *testing.T) {
		t.Run("creates parents when asked to", func(t *testing.T) {
			root := New()

			require.NoError(t, root.InsertNode("parent-1/directory", true, true))
			require.NoError(t, root.InsertNode("parent-1/file", true, false))

			require.NoError(t, root.InsertNode("parent-2/file", true, false))
			require.NoError(t, root.InsertNode("parent-2/directory", true, true))

			require.Equal(t,
				&Node{
					children: children{
						"parent-1": {
							children{
								"directory": {children: children{}},
								"file":      {},
							},
						},
						"parent-2": {
							children{
								"file":      {},
								"directory": {children: children{}},
							},
						},
					},
				},
				root,
			)
		})

		t.Run("does not creates parents when not asked to", func(t *testing.T) {
			root := New()
			require.Equal(t, errParentNotFound, root.InsertNode("parent-1/directory", false, true))
			require.Equal(t, errParentNotFound, root.InsertNode("parent-1/file", false, false))
		})

		t.Run("fails if parent is not a directory", func(t *testing.T) {
			root := New()
			require.NoError(t, root.InsertNode("refs", false, false))
			require.Equal(t, errParentIsNotDirectory, root.InsertNode("refs/file", false, false))
		})

		t.Run("fails if target node already exists", func(t *testing.T) {
			root := New()
			require.NoError(t, root.InsertNode("refs/heads/directory", true, true))
			require.NoError(t, root.InsertNode("refs/heads/file", false, false))

			require.Equal(t,
				errTargetAlreadyExists,
				root.InsertReference("refs/heads/directory"),
			)

			require.Equal(t,
				errTargetAlreadyExists,
				root.InsertReference("refs/heads/file"),
			)
		})
	})

	t.Run("RemoveNode", func(t *testing.T) {
		t.Run("fails if target does not exist", func(t *testing.T) {
			root := New()
			require.Equal(t, errTargetNotFound, root.RemoveNode("refs"))
		})

		t.Run("fails if parent does not exist", func(t *testing.T) {
			root := New()
			require.Equal(t, errParentNotFound, root.RemoveNode("refs/heads"))
		})

		t.Run("fails if target has children", func(t *testing.T) {
			root := New()
			require.NoError(t, root.InsertNode("refs/heads/main", true, false))
			require.Equal(t, errTargetHasChildren, root.RemoveNode("refs/heads"))
		})

		t.Run("successfully removes a node", func(t *testing.T) {
			root := New()
			require.NoError(t, root.InsertNode("refs/heads/main", true, false))
			require.NoError(t, root.RemoveNode("refs/heads/main"))
			require.Equal(t,
				&Node{
					children: children{
						"refs": {
							children{
								"heads": {children: children{}},
							},
						},
					},
				},
				root,
			)
		})
	})

	root := New()

	for _, ref := range []string{
		"refs/root-branch-1",
		"refs/root-branch-2",
		"refs/heads/branch-1",
		"refs/heads/branch-2",
		"refs/heads/subdir/branch-3",
		"refs/heads/subdir/branch-4",
		"refs/tags/tag-1",
		"refs/tags/tag-2",
		"refs/tags/subdir/tag-3",
		"refs/tags/subdir/tag-4",
	} {
		require.NoError(t, root.InsertReference(ref))
	}

	require.Equal(t,
		&Node{
			children: children{
				"refs": {
					children{
						"root-branch-1": {},
						"root-branch-2": {},
						"heads": {
							children: children{
								"branch-1": {},
								"branch-2": {},
								"subdir": {
									children: children{
										"branch-3": {},
										"branch-4": {},
									},
								},
							},
						},
						"tags": {
							children: children{
								"tag-1": {},
								"tag-2": {},
								"subdir": {
									children: children{
										"tag-3": {},
										"tag-4": {},
									},
								},
							},
						},
					},
				},
			},
		},
		root,
	)

	t.Run("Contains", func(t *testing.T) {
		t.Parallel()

		for _, entry := range []struct {
			reference string
			contains  bool
			isDir     bool
		}{
			{reference: "refs", contains: true},
			{reference: "refs/root-branch-1", contains: true},
			{reference: "refs/root-branch-2", contains: true},
			{reference: "refs/heads", contains: true},
			{reference: "refs/heads/branch-1", contains: true},
			{reference: "refs/heads/branch-2", contains: true},
			{reference: "refs/heads/subdir", contains: true},
			{reference: "refs/heads/subdir/branch-3", contains: true},
			{reference: "refs/heads/subdir/branch-4", contains: true},
			{reference: "refs/tags", contains: true},
			{reference: "refs/tags/tag-1", contains: true},
			{reference: "refs/tags/tag-2", contains: true},
			{reference: "refs/tags/subdir", contains: true},
			{reference: "refs/tags/subdir/tag-3", contains: true},
			{reference: "refs/tags/subdir/tag-4", contains: true},
			{reference: "non-existent"},
			{reference: "refs/non-existent"},
			{reference: "refs/heads/non-existent"},
			{reference: "refs/heads/subdir/non-existent"},
			{reference: "refs/heads/subdir/branch-4/non-existent"},
			{reference: "refs/tags/non-existent"},
			{reference: "refs/tags/subdir/non-existent"},
			{reference: "refs/tags/subdir/tag-4/non-existent"},
		} {
			require.Equal(t, root.Contains(entry.reference), entry.contains)
		}
	})

	t.Run("walk", func(t *testing.T) {
		t.Parallel()

		type result struct {
			path  string
			isDir bool
		}

		errSentinel := errors.New("sentinel")

		for _, tc := range []struct {
			desc            string
			walk            func(WalkCallback) error
			pathToFailOn    string
			expectedResults []result
		}{
			{
				desc: "pre-order",
				walk: root.WalkPreOrder,
				expectedResults: []result{
					{path: "refs", isDir: true},
					{path: "refs/heads", isDir: true},
					{path: "refs/heads/branch-1"},
					{path: "refs/heads/branch-2"},
					{path: "refs/heads/subdir", isDir: true},
					{path: "refs/heads/subdir/branch-3"},
					{path: "refs/heads/subdir/branch-4"},
					{path: "refs/root-branch-1"},
					{path: "refs/root-branch-2"},
					{path: "refs/tags", isDir: true},
					{path: "refs/tags/subdir", isDir: true},
					{path: "refs/tags/subdir/tag-3"},
					{path: "refs/tags/subdir/tag-4"},
					{path: "refs/tags/tag-1"},
					{path: "refs/tags/tag-2"},
				},
			},
			{
				desc:         "pre-order failure on directory",
				walk:         root.WalkPreOrder,
				pathToFailOn: "refs/heads/subdir",
				expectedResults: []result{
					{path: "refs", isDir: true},
					{path: "refs/heads", isDir: true},
					{path: "refs/heads/branch-1"},
					{path: "refs/heads/branch-2"},
					{path: "refs/heads/subdir", isDir: true},
				},
			},
			{
				desc:         "pre-order failure on file",
				walk:         root.WalkPreOrder,
				pathToFailOn: "refs/heads/subdir/branch-3",
				expectedResults: []result{
					{path: "refs", isDir: true},
					{path: "refs/heads", isDir: true},
					{path: "refs/heads/branch-1"},
					{path: "refs/heads/branch-2"},
					{path: "refs/heads/subdir", isDir: true},
					{path: "refs/heads/subdir/branch-3"},
				},
			},
			{
				desc: "post-order",
				walk: root.WalkPostOrder,
				expectedResults: []result{
					{path: "refs/heads/branch-1"},
					{path: "refs/heads/branch-2"},
					{path: "refs/heads/subdir/branch-3"},
					{path: "refs/heads/subdir/branch-4"},
					{path: "refs/heads/subdir", isDir: true},
					{path: "refs/heads", isDir: true},
					{path: "refs/root-branch-1"},
					{path: "refs/root-branch-2"},
					{path: "refs/tags/subdir/tag-3"},
					{path: "refs/tags/subdir/tag-4"},
					{path: "refs/tags/subdir", isDir: true},
					{path: "refs/tags/tag-1"},
					{path: "refs/tags/tag-2"},
					{path: "refs/tags", isDir: true},
					{path: "refs", isDir: true},
				},
			},
			{
				desc:         "post-order failure on directory",
				walk:         root.WalkPostOrder,
				pathToFailOn: "refs/heads/subdir",
				expectedResults: []result{
					{path: "refs/heads/branch-1"},
					{path: "refs/heads/branch-2"},
					{path: "refs/heads/subdir/branch-3"},
					{path: "refs/heads/subdir/branch-4"},
					{path: "refs/heads/subdir", isDir: true},
				},
			},
			{
				desc:         "post-order failure on file",
				walk:         root.WalkPostOrder,
				pathToFailOn: "refs/heads/subdir/branch-3",
				expectedResults: []result{
					{path: "refs/heads/branch-1"},
					{path: "refs/heads/branch-2"},
					{path: "refs/heads/subdir/branch-3"},
				},
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				t.Parallel()

				var actualResults []result
				err := tc.walk(func(path string, isDir bool) error {
					actualResults = append(actualResults, result{
						path:  path,
						isDir: isDir,
					})

					if path == tc.pathToFailOn {
						return errSentinel
					}

					return nil
				})

				if tc.pathToFailOn != "" {
					require.ErrorIs(t, err, errSentinel)
				} else {
					require.NoError(t, err)
				}

				require.Equal(t, tc.expectedResults, actualResults)
			})
		}
	})

	t.Run("walking empty tree", func(t *testing.T) {
		t.Run("post-order", func(t *testing.T) {
			require.NoError(t, New().WalkPostOrder(func(string, bool) error {
				t.Fatalf("callback should not be invoked on an empty tree.")
				return nil
			}))
		})

		t.Run("pre-order", func(t *testing.T) {
			require.NoError(t, New().WalkPreOrder(func(string, bool) error {
				t.Fatalf("callback should not be invoked on an empty tree.")
				return nil
			}))
		})
	})
}
