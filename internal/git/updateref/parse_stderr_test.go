package updateref

import (
	"bytes"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/stretchr/testify/require"
)

// TestUpdater_parseStderr checks that lock errors are classified per reference backend. The MVCC
// backend is derived from reftables and reports its table locks the same way, so it must be
// classified like reftables, not like the files backend.
func TestUpdater_parseStderr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		backend  git.ReferenceBackend
		stderr   string
		expected error
	}{
		{
			desc:     "reftable table lock",
			backend:  git.ReferenceBackendReftables,
			stderr:   "fatal: prepare: cannot lock references\n",
			expected: AlreadyLockedError{},
		},
		{
			desc:     "mvcc table lock on prepare",
			backend:  git.ReferenceBackendMVCC,
			stderr:   "fatal: prepare: cannot lock references\n",
			expected: AlreadyLockedError{},
		},
		{
			desc:     "mvcc table lock on commit",
			backend:  git.ReferenceBackendMVCC,
			stderr:   "fatal: commit: cannot lock references\n",
			expected: AlreadyLockedError{},
		},
		{
			desc:     "files reference lock keeps the reference name",
			backend:  git.ReferenceBackendFiles,
			stderr:   "fatal: prepare: cannot lock ref 'refs/heads/main': Unable to create '/repo/refs/heads/main.lock': File exists.\n",
			expected: AlreadyLockedError{ReferenceName: "refs/heads/main"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			u := &Updater{
				referenceBackend: tc.backend,
				stderr:           bytes.NewBufferString(tc.stderr),
			}
			require.Equal(t, tc.expected, u.parseStderr())
		})
	}
}
