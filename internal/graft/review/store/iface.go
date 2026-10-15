package store

import "github.com/feder-cr/open_scion/proto/go/gitalypb"

// ReviewStore is the storage of the reviews of one repository: the files of
// the repository (*Store), or the storage core's database in authoritative
// mode (internal/graft/corestore/dbreview). Every implementation has exactly
// the semantics documented on the methods of *Store, including ErrNotFound.
type ReviewStore interface {
	Create(r *gitalypb.Review) (*gitalypb.Review, error)
	Get(id string) (*gitalypb.Review, error)
	List(targetBranch string, includeClosed bool, limit int) ([]*gitalypb.Review, error)
	Update(id string, fn func(*gitalypb.Review) error) (*gitalypb.Review, error)
}

var _ ReviewStore = (*Store)(nil)
