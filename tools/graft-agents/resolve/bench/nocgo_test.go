//go:build !cgo

package bench

import (
	"context"
	"errors"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// TestMineRefusesWithoutCgo: mining checks the syntax of the human resolutions, so a build without
// the grammars refuses to mine instead of keeping references it cannot validate.
func TestMineRefusesWithoutCgo(t *testing.T) {
	if _, _, err := Mine(context.Background(), Repo{Name: "x"}, t.TempDir(), MineOptions{}); !errors.Is(err, syntax.ErrUnavailable) {
		t.Fatalf("err = %v, want syntax.ErrUnavailable", err)
	}
	if _, _, err := Filter(context.Background(), nil, nil); !errors.Is(err, syntax.ErrUnavailable) {
		t.Fatalf("Filter: err = %v, want syntax.ErrUnavailable", err)
	}
}
