package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Spec: ids are decimal counters from next_id; Create assigns id and timestamps.
func TestSpecStoreCreateAssignsIncreasingDecimalIDs(t *testing.T) {
	s := Open(t.TempDir())

	var prev int
	for i := 0; i < 3; i++ {
		r, err := s.Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t")})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		n, err := strconv.Atoi(r.GetId())
		if err != nil {
			t.Fatalf("id %q is not a decimal counter: %v", r.GetId(), err)
		}
		if i > 0 && n <= prev {
			t.Fatalf("ids not strictly increasing: %d after %d", n, prev)
		}
		prev = n
		if r.GetCreatedAt() == nil || r.GetUpdatedAt() == nil {
			t.Fatalf("Create must set created_at and updated_at")
		}
	}
}

// Spec: Get returns ErrNotFound for an unknown id; Update too.
func TestSpecStoreMissingReviewIsErrNotFound(t *testing.T) {
	s := Open(t.TempDir())

	if _, err := s.Get("424242"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown id: want ErrNotFound, got %v", err)
	}
	_, err := s.Update("424242", func(*gitalypb.Review) error { return nil })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update unknown id: want ErrNotFound, got %v", err)
	}
}

// Spec: List filters by target branch, hides closed reviews unless
// includeClosed, returns newest first and honours limit.
func TestSpecStoreListNewestFirstAndFilters(t *testing.T) {
	s := Open(t.TempDir())

	a, err := s.Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(&gitalypb.Review{TargetBranch: []byte("dev"), Title: []byte("c")}); err != nil {
		t.Fatal(err)
	}

	ids := func(rs []*gitalypb.Review) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.GetId())
		}
		return out
	}

	got, err := s.List("main", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{b.GetId(), a.GetId()}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("List main: want newest first %v, got %v", want, ids(got))
	}

	if _, err := s.Update(b.GetId(), func(r *gitalypb.Review) error {
		r.State = gitalypb.Review_STATE_CLOSED
		return nil
	}); err != nil {
		t.Fatalf("Update to closed: %v", err)
	}

	got, err = s.List("main", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetId() != a.GetId() {
		t.Fatalf("List without closed: want only %s, got %v", a.GetId(), ids(got))
	}

	got, err = s.List("main", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{b.GetId(), a.GetId()}; strings.Join(ids(got), ",") != strings.Join(want, ",") {
		t.Fatalf("List main includeClosed: want %v, got %v", want, ids(got))
	}

	got, err = s.List("main", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetId() != b.GetId() {
		t.Fatalf("List limit 1: want newest %s, got %v", b.GetId(), ids(got))
	}
}

// Spec: all mutations are serialised per repository (mutex plus flock); 20
// concurrent creates must yield 20 stored reviews with unique ids.
func TestSpecStoreConcurrentCreateUniqueIDs(t *testing.T) {
	s := Open(t.TempDir())

	const n = 20
	var wg sync.WaitGroup
	idsCh := make(chan string, n)
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("concurrent")})
			if err != nil {
				errCh <- err
				return
			}
			idsCh <- r.GetId()
		}()
	}
	wg.Wait()
	close(idsCh)
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent Create: %v", err)
	}
	seen := map[string]bool{}
	for id := range idsCh {
		if seen[id] {
			t.Fatalf("duplicate review id %s", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("want %d unique ids, got %d", n, len(seen))
	}

	got, err := s.List("main", true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("want %d stored reviews, got %d", n, len(got))
	}
}

// Spec: the on-disk review <id>.json omits requirements and approved, which
// are recomputed on read. The directory location is not pinned by the spec,
// so the test searches the tree under the repo path for <id>.json.
func TestSpecStoreFileOmitsRecomputedFields(t *testing.T) {
	repo := t.TempDir()
	s := Open(repo)

	r, err := s.Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("persist me")})
	if err != nil {
		t.Fatal(err)
	}

	var found string
	walkErr := filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Base(p) == r.GetId()+".json" {
			found = p
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if found == "" {
		t.Fatalf("no %s.json written under repo path", r.GetId())
	}
	if !strings.Contains(filepath.ToSlash(found), "graft/reviews/") {
		t.Fatalf("review file %s is not under graft/reviews/", found)
	}

	data, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "requirements") || strings.Contains(string(data), "\"approved\"") {
		t.Fatalf("stored file must not contain requirements or approved: %s", data)
	}
}
