package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/feder-cr/open_scion/tools/graft-agents/protocol"
)

// rerere reuses earlier resolutions, like git rerere. The key is the SHA-256
// of the three contents (and modes) of the conflict, so the same conflict in
// any path is recognized. Each entry is one JSON file named after the key,
// which keeps keys out of path handling: the key is always 64 hex digits.
//
// Lookup answers only conflicts it has seen; Record stores every answer the
// chain accepted (after checks). Writes are atomic renames, so concurrent
// processes sharing a directory cannot read a partial entry.
type rerere struct {
	dir string
}

type storedResolution struct {
	Action  string `json:"action"`
	Mode    string `json:"mode,omitempty"`
	Content []byte `json:"content"`
}

func newRerere(spec StrategySpec) (Strategy, error) {
	if err := allowOnly(spec, "dir"); err != nil {
		return nil, err
	}
	dir, err := optString(spec, "dir")
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, fmt.Errorf("strategy %q: rerere needs dir", spec.Name)
	}
	return rerere{dir: dir}, nil
}

// rerereKey hashes the three sides. Absent sides hash differently from empty
// ones.
func rerereKey(f File) string {
	h := sha256.New()
	h.Write([]byte("graft-rerere-v1\x00"))
	for _, s := range []*protocol.Side{f.Base, f.Ours, f.Theirs} {
		if s == nil {
			h.Write([]byte("-\x00"))
			continue
		}
		h.Write([]byte("+" + s.Mode + "\x00" + strconv.Itoa(len(s.Content)) + "\x00"))
		h.Write(s.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r rerere) entryPath(key string) string {
	return filepath.Join(r.dir, key+".json")
}

func (r rerere) Resolve(_ context.Context, f File, _ protocol.Request) (Resolution, bool, error) {
	data, err := os.ReadFile(r.entryPath(rerereKey(f)))
	if errors.Is(err, fs.ErrNotExist) {
		return Resolution{}, false, nil
	}
	if err != nil {
		return Resolution{}, false, fmt.Errorf("reading rerere entry: %w", err)
	}
	var s storedResolution
	if err := json.Unmarshal(data, &s); err != nil {
		return Resolution{}, false, fmt.Errorf("corrupt rerere entry: %w", err)
	}
	res := Resolution{Action: s.Action, Mode: s.Mode, Content: s.Content}
	switch s.Action {
	case protocol.ActionDelete:
		res.Mode, res.Content = "", nil
	case protocol.ActionWrite:
		if !protocol.ValidMode(s.Mode) {
			return Resolution{}, false, fmt.Errorf("corrupt rerere entry: invalid mode %q", s.Mode)
		}
	default:
		return Resolution{}, false, fmt.Errorf("corrupt rerere entry: unknown action %q", s.Action)
	}
	return res, true, nil
}

// Record stores an accepted resolution.
func (r rerere) Record(_ context.Context, f File, res Resolution) error {
	data, err := json.Marshal(storedResolution{Action: res.Action, Mode: res.Mode, Content: res.Content})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return fmt.Errorf("creating rerere directory: %w", err)
	}
	tmp, err := os.CreateTemp(r.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating rerere entry: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing rerere entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing rerere entry: %w", err)
	}
	if err := os.Rename(tmpName, r.entryPath(rerereKey(f))); err != nil {
		return fmt.Errorf("storing rerere entry: %w", err)
	}
	return nil
}
