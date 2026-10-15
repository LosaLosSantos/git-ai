package git

import (
	"fmt"
	"regexp"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

var (
	// ReferenceBackendReftables denotes the new binary format based
	// reftable backend.
	// See https://www.git-scm.com/docs/reftable for more details.
	ReferenceBackendReftables = ReferenceBackend{
		Name: "reftable",
		updateRefErrorRegexs: updateRefErrorRegexs{
			InTransactionConflictRegex:   regexp.MustCompile(`^fatal: .*: cannot process '(.*)' and '(.*)' at the same time\n$`),
			MismatchingStateRegex:        regexp.MustCompile(`^fatal: (?:.*: )?cannot lock ref '(.*)': is at (.*) but expected (.*)\n$`),
			MultipleUpdatesRegex:         regexp.MustCompile(`^fatal: .*: multiple updates for ref '(.*)' not allowed\n$`),
			NonCommitObjectRegex:         regexp.MustCompile(`^fatal: (?:.*: )?trying to write non-commit object (.*) to branch '(.*)'\n`),
			NonExistentObjectRegex:       regexp.MustCompile(`^fatal: (?:.*: )?trying to write ref '(.*)' with nonexistent object (.*)\n$`),
			RefInvalidFormatRegex:        regexp.MustCompile(`^fatal: invalid ref format: (.*)\n$`),
			RefLockedRegex:               regexp.MustCompile(`^fatal: (prepare|commit): cannot lock references\n`),
			ReferenceAlreadyExistsRegex:  regexp.MustCompile(`^fatal: .*: cannot lock ref '(.*)': reference already exists\n$`),
			ReferenceExistsConflictRegex: regexp.MustCompile(`^fatal: .*: '(.*)' exists; cannot create '(.*)'\n$`),
			// Reftable uses the same error as RefLockedRegex when there is an
			// existing tables.list.lock present in the repo.
			PackedRefsLockedRegex: regexp.MustCompile(""),
		},
	}

	// ReferenceBackendMVCC denotes the MVCC-enabled reference backend. It is
	// served by a dedicated, MVCC-capable Git binary that diverges from upstream
	// Git. The error formats are inherited from the reftable backend the MVCC
	// backend is derived from.
	ReferenceBackendMVCC = ReferenceBackend{
		Name:                 "mvcc",
		updateRefErrorRegexs: ReferenceBackendReftables.updateRefErrorRegexs,
	}

	// ReferenceBackendFiles denotes the traditional filesystem based
	// reference backend.
	ReferenceBackendFiles = ReferenceBackend{
		Name: "files",
		updateRefErrorRegexs: updateRefErrorRegexs{
			InTransactionConflictRegex:   regexp.MustCompile(`^fatal: .*: cannot process '(.*)' and '(.*)' at the same time\n$`),
			MismatchingStateRegex:        regexp.MustCompile(`^fatal: (?:.*: )?cannot lock ref '(.*)': is at (.*) but expected (.*)\n$`),
			MultipleUpdatesRegex:         regexp.MustCompile(`^fatal: .*: multiple updates for ref '(.*)' not allowed\n$`),
			NonCommitObjectRegex:         regexp.MustCompile(`^fatal: (?:.*: cannot update ref '.*': )?trying to write non-commit object (.*) to branch '(.*)'\n`),
			NonExistentObjectRegex:       regexp.MustCompile(`^fatal: (?:.*: cannot update ref '.*': )?trying to write ref '(.*)' with nonexistent object (.*)\n$`),
			RefInvalidFormatRegex:        regexp.MustCompile(`^fatal: invalid ref format: (.*)\n$`),
			RefLockedRegex:               regexp.MustCompile(`^fatal: (prepare|commit): cannot lock ref '(.+?)': Unable to create '.*': File exists.`),
			ReferenceAlreadyExistsRegex:  regexp.MustCompile(`^fatal: .*: cannot lock ref '(.*)': reference already exists\n$`),
			ReferenceExistsConflictRegex: regexp.MustCompile(`^fatal: .*: '(.*)' exists; cannot create '(.*)'\n$`),
			PackedRefsLockedRegex:        regexp.MustCompile(`(packed-refs\.lock': File exists\.\n)|(packed-refs\.new: File exists\n$)`),
		},
	}
)

// updateRefErrorRegexs holds the patterns that parse the stderr of git-update-ref(1) for a
// reference backend. ReferenceExistsConflictRegex must match the file/directory conflict both with
// and without a "cannot lock ref '...': " prefix: the files backend adds it when the conflict is
// found while locking the loose reference, but not when it is found by the deferred check against
// packed references, e.g. when deleting a reference that does not exist. The greedy `.*: ` absorbs
// the prefix when present, as reference names cannot contain a colon.
type updateRefErrorRegexs struct {
	InTransactionConflictRegex   *regexp.Regexp
	MismatchingStateRegex        *regexp.Regexp
	MultipleUpdatesRegex         *regexp.Regexp
	NonCommitObjectRegex         *regexp.Regexp
	NonExistentObjectRegex       *regexp.Regexp
	RefInvalidFormatRegex        *regexp.Regexp
	RefLockedRegex               *regexp.Regexp
	ReferenceAlreadyExistsRegex  *regexp.Regexp
	ReferenceExistsConflictRegex *regexp.Regexp
	PackedRefsLockedRegex        *regexp.Regexp
}

// ReferenceBackend is the specific backend implementation of references.
type ReferenceBackend struct {
	updateRefErrorRegexs
	// Name is the name of the reference backend.
	Name string
}

// ReferenceBackendByName maps the output of `rev-parse --show-ref-format` to
// Gitaly's internal reference backend types.
func ReferenceBackendByName(name string) (ReferenceBackend, error) {
	switch name {
	case ReferenceBackendReftables.Name:
		return ReferenceBackendReftables, nil
	case ReferenceBackendMVCC.Name:
		return ReferenceBackendMVCC, nil
	case ReferenceBackendFiles.Name:
		return ReferenceBackendFiles, nil
	default:
		return ReferenceBackend{}, fmt.Errorf("unknown reference backend: %q", name)
	}
}

// ReferenceBackendByProto maps a reference backend as sent over the wire onto Gitaly's internal
// types. An unspecified backend yields the zero value. That means the caller has no opinion, so
// the node picks.
func ReferenceBackendByProto(backend gitalypb.ReferenceBackend) (ReferenceBackend, error) {
	switch backend {
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_UNSPECIFIED:
		return ReferenceBackend{}, nil
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES:
		return ReferenceBackendFiles, nil
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE:
		return ReferenceBackendReftables, nil
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_MVCC:
		return ReferenceBackendMVCC, nil
	default:
		return ReferenceBackend{}, fmt.Errorf("unknown reference backend: %q", backend)
	}
}

// ReferenceBackendToProto is the inverse of ReferenceBackendByProto. The zero value maps to an
// unspecified backend.
func ReferenceBackendToProto(backend ReferenceBackend) (gitalypb.ReferenceBackend, error) {
	switch backend.Name {
	case "":
		return gitalypb.ReferenceBackend_REFERENCE_BACKEND_UNSPECIFIED, nil
	case ReferenceBackendFiles.Name:
		return gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES, nil
	case ReferenceBackendReftables.Name:
		return gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE, nil
	case ReferenceBackendMVCC.Name:
		return gitalypb.ReferenceBackend_REFERENCE_BACKEND_MVCC, nil
	default:
		return gitalypb.ReferenceBackend_REFERENCE_BACKEND_UNSPECIFIED, fmt.Errorf("unknown reference backend: %q", backend.Name)
	}
}
