package git

import (
	"os"
	"path/filepath"
	"regexp"
)

// PackFileRegexCore is the regex that matches the common parts of pack and idx filenames.
const PackFileRegexCore = `(.*/pack-)([0-9a-f]{40}|[0-9a-f]{64})`

// ListPackfiles returns the packfiles in objDir.
func ListPackfiles(objDir string) ([]string, error) {
	packFileRegex := regexp.MustCompile(`\A` + PackFileRegexCore + `\.pack\z`)
	packDir := filepath.Join(objDir, "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return nil, err
	}

	var packs []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}

		if p := filepath.Join(packDir, ent.Name()); packFileRegex.MatchString(p) {
			packs = append(packs, p)
		}
	}

	return packs, nil
}
