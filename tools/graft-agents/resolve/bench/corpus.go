// Package bench measures the strategies of graft-resolve on real merge conflicts: it mines the
// conflicts of the merge commits of pinned repositories (with the human resolution of each), and
// replays them through a configured chain, judging every accepted result against the human one.
//
// Only a manifest of the mined cases is kept (repositories, commit and object IDs, paths,
// licenses); the contents are read from the clones at run time, so no third-party code is ever
// stored with the benchmark.
package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// Families of the benchmark, by the extension of the conflicted path.
const (
	FamilyTSJS     = "ts-js"
	FamilyPython   = "python"
	FamilyGo       = "go"
	FamilyRustJava = "rust-java"
)

// familyOf returns the family of a path by its extension, or "" for other files.
// LanguageOf returns the language of a benchmark path, by extension: typescript, javascript,
// python, go, rust or java; "" for any other path.
func LanguageOf(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py", ".pyi":
		return "python"
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	}
	return ""
}

// stratumOf returns the sampling stratum of a case: its family, except that Rust and Java,
// one family, are two strata, so that the few Rust cases are not drowned by the Java ones.
func stratumOf(c Case) string {
	if c.Family == FamilyRustJava {
		if LanguageOf(c.Path) == "rust" {
			return "rust"
		}
		return "java"
	}
	return c.Family
}

func familyOf(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return FamilyTSJS
	case ".py", ".pyi":
		return FamilyPython
	case ".go":
		return FamilyGo
	case ".rs", ".java":
		return FamilyRustJava
	}
	return ""
}

// Repo is one repository of the corpus.
type Repo struct {
	// Name identifies the repository in the manifest and the report, for example
	// "golang/go".
	Name string `json:"name"`
	// URL is the clone URL.
	URL string `json:"url"`
	// Commit pins the history: the merges reachable from it are mined.
	Commit string `json:"commit"`
	// License is the SPDX identifier of the repository's license. The corpus only lists
	// permissively licensed repositories.
	License string `json:"license"`
	// MaxMerges bounds the merges that are replayed, newest first (0: the miner's default).
	MaxMerges int `json:"max_merges,omitempty"`
}

// Corpus is the committed list of repositories.
type Corpus struct {
	Repos []Repo `json:"repos"`
}

// permissiveLicenses are the licenses a corpus repository may have.
var permissiveLicenses = map[string]bool{
	"MIT": true, "Apache-2.0": true, "BSD-2-Clause": true, "BSD-3-Clause": true,
	"ISC": true, "PSF-2.0": true, "MIT OR Apache-2.0": true, "Apache-2.0 OR MIT": true,
}

// LoadCorpus reads and validates a corpus file.
func LoadCorpus(file string) (Corpus, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Corpus{}, err
	}
	var c Corpus
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Corpus{}, fmt.Errorf("corpus %s: %w", file, err)
	}
	if len(c.Repos) == 0 {
		return Corpus{}, errors.New("corpus lists no repositories")
	}
	names := map[string]bool{}
	for _, r := range c.Repos {
		switch {
		case r.Name == "" || r.URL == "" || r.Commit == "":
			return Corpus{}, fmt.Errorf("corpus repository %q needs a name, a URL and a commit", r.Name)
		case names[r.Name]:
			return Corpus{}, fmt.Errorf("corpus lists %q twice", r.Name)
		case !permissiveLicenses[r.License]:
			return Corpus{}, fmt.Errorf("corpus repository %q has license %q, not a permissive one", r.Name, r.License)
		case strings.ContainsAny(r.Name, " \t\n") || strings.Contains(r.Name, ".."):
			return Corpus{}, fmt.Errorf("corpus repository name %q is not a plain name", r.Name)
		}
		names[r.Name] = true
	}
	return c, nil
}

// Case is one conflicting file of one replayed merge, with the human resolution.
type Case struct {
	// ID is stable across runs: the repository, the merge commit and the path.
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	License string `json:"license"`
	Family  string `json:"family"`
	// Merge is the merge commit; Ours and Theirs its first and second parent; Base their merge
	// base.
	Merge  string `json:"merge"`
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
	Base   string `json:"base"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	// The blobs of each side, nil when the side is absent; Human is the merge commit's version,
	// nil when the human resolution deleted the file.
	BaseBlob   *entry `json:"base_blob"`
	OursBlob   *entry `json:"ours_blob"`
	TheirsBlob *entry `json:"theirs_blob"`
	HumanBlob  *entry `json:"human_blob"`
}

// Manifest is the result of mining: the cases and what was skipped.
type Manifest struct {
	Version int    `json:"version"`
	Cases   []Case `json:"cases"`
	// Stats holds the counts of the miner per repository.
	Stats map[string]MineStats `json:"stats"`
}

// ManifestVersion is the format of the manifest.
const ManifestVersion = 1

// SaveManifest writes the manifest as indented JSON.
func SaveManifest(file string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Written under a temporary name and renamed, so that a stopped run never leaves a truncated
	// manifest (bench mine resumes from the manifests of the repositories already mined).
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// LoadManifest reads a manifest.
func LoadManifest(file string) (Manifest, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest %s: %w", file, err)
	}
	if m.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf("manifest %s has version %d, want %d", file, m.Version, ManifestVersion)
	}
	return m, nil
}
