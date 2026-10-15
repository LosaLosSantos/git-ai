package linguist

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/mode"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestInstance_Stats(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	setupStaleCacheTest := func(staleName string) func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
		return func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})

			commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
				gittest.TreeEntry{Path: "application.rb", Mode: "100644", Content: strings.Repeat("a", 2943)},
			))
			repo := localrepo.NewTestRepo(t, cfg, repoProto)

			// Populate the cache with correct stats.
			_, err := New(cfg, testhelper.NewLogger(t), catfileCache, repo).Stats(ctx, commitID)
			require.NoError(t, err)
			require.FileExists(t, filepath.Join(repoPath, languageStatsFilename))

			// Tamper the cache: replace canonical "Ruby" with the stale name.
			stats, err := initLanguageStats(ctx, repo)
			require.NoError(t, err)
			require.Contains(t, stats.Totals, "Ruby")

			rubyBytes := stats.Totals["Ruby"]
			delete(stats.Totals, "Ruby")
			stats.Totals[staleName] = rubyBytes

			for filename, byFile := range stats.ByFile {
				if _, ok := byFile["Ruby"]; ok {
					b := byFile["Ruby"]
					delete(byFile, "Ruby")
					byFile[staleName] = b
					stats.ByFile[filename] = byFile
				}
			}

			require.NoError(t, stats.save(ctx, repo, commitID))

			return repoProto, repoPath, commitID
		}
	}

	for _, tc := range []struct {
		desc          string
		setup         func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID)
		expectedStats ByteCountPerLanguage
		expectedErr   string
	}{
		{
			desc: "successful",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "webpack.coffee", Mode: "100644", Content: strings.Repeat("a", 107)},
					gittest.TreeEntry{Path: "show_user.html", Mode: "100644", Content: strings.Repeat("a", 349)},
					gittest.TreeEntry{Path: "api.javascript", Mode: "100644", Content: strings.Repeat("a", 1014)},
					gittest.TreeEntry{Path: "application.rb", Mode: "100644", Content: strings.Repeat("a", 2943)},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"CoffeeScript": 107,
				"HTML":         349,
				"JavaScript":   1014,
				"Ruby":         2943,
			},
		},
		{
			desc: "documentation is ignored",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				docTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "readme.md", Mode: "100644", Content: strings.Repeat("a", 500)},
					{Path: "index.html", Mode: "100644", Content: strings.Repeat("a", 120)},
					{Path: "formatter.rb", Mode: "100644", Content: strings.Repeat("a", 403)},
				})
				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "docs", Mode: "040000", OID: docTree},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C": 85,
			},
		},
		{
			desc: "documentation with overrides",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				docTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "readme.md", Mode: "100644", Content: strings.Repeat("a", 500)},
					{Path: "index.html", Mode: "100644", Content: strings.Repeat("a", 120)},
					{Path: "formatter.rb", Mode: "100644", Content: strings.Repeat("a", 403)},
				})
				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "docs", Mode: "040000", OID: docTree},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
					gittest.TreeEntry{Path: ".gitattributes", Mode: "100644", Content: "formatter.rb -linguist-documentation"},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C":    85,
				"Ruby": 403,
			},
		},
		{
			desc: "vendor is ignored",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				vendorTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "app.rb", Mode: "100644", Content: strings.Repeat("a", 500)},
				})
				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "vendor", Mode: "040000", OID: vendorTree},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C": 85,
			},
		},
		{
			desc: "vendor with overrides",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				vendorTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "app.rb", Mode: "100644", Content: strings.Repeat("a", 500)},
				})
				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "vendor", Mode: "040000", OID: vendorTree},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
					gittest.TreeEntry{Path: ".gitattributes", Mode: "100644", Content: "*.rb -linguist-vendored"},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C":    85,
				"Ruby": 500,
			},
		},
		{
			desc: "generated is ignored",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				podsTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "app.swift", Mode: "100644", Content: strings.Repeat("a", 500)},
				})
				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "Pods", Mode: "040000", OID: podsTree},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C": 85,
			},
		},
		{
			desc: "generated with overrides",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				podsTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "app.swift", Mode: "100644", Content: strings.Repeat("a", 500)},
				})
				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "Pods", Mode: "040000", OID: podsTree},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
					gittest.TreeEntry{Path: ".gitattributes", Mode: "100644", Content: "Pods/* -linguist-generated"},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C":     85,
				"Swift": 500,
			},
		},
		{
			desc: "undetectable languages are ignored",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "config.json", Mode: "100644", Content: strings.Repeat("a", 234)},
					gittest.TreeEntry{Path: "manual.md", Mode: "100644", Content: strings.Repeat("a", 553)},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C": 85,
			},
		},
		{
			desc: "undetectable languages with overrides",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "config.json", Mode: "100644", Content: strings.Repeat("a", 234)},
					gittest.TreeEntry{Path: "manual.md", Mode: "100644", Content: strings.Repeat("a", 553)},
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 85)},
					gittest.TreeEntry{
						Path: ".gitattributes",
						Mode: "100644",
						Content: "*.md linguist-detectable\n" +
							"*.json linguist-detectable\n",
					},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C":        85,
				"JSON":     234,
				"Markdown": 553,
			},
		},
		{
			desc: "file specific documentation override",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				docTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "readme.md", Mode: "100644", Content: strings.Repeat("a", 500)},
					{Path: "index.html", Mode: "100644", Content: strings.Repeat("a", 120)},
					{Path: "formatter.rb", Mode: "100644", Content: strings.Repeat("a", 403)},
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "docu", Mode: "040000", OID: docTree},
					gittest.TreeEntry{
						Path: ".gitattributes",
						Mode: "100644",
						Content: "docu/* linguist-documentation\n" +
							"docu/formatter.rb -linguist-documentation",
					},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"Ruby": 403,
			},
		},
		{
			desc: "detectable overrides",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "keeb.kicad_pcb", Mode: "100644", Content: strings.Repeat("a", 500)},
					gittest.TreeEntry{Path: "keeb.sch", Mode: "100644", Content: strings.Repeat("a", 120)},
					gittest.TreeEntry{Path: "export_bom.py", Mode: "100644", Content: strings.Repeat("a", 403)},
					gittest.TreeEntry{
						Path: ".gitattributes",
						Mode: "100644",
						Content: "*.kicad_pcb linguist-detectable\n" +
							"*.sch linguist-detectable\n" +
							"export_bom.py -linguist-detectable",
					},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"KiCad Layout": 500,
				"XML":          120,
			},
		},
		{
			desc: "double star file pattern documentation override",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				subSubTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "first.rb", Mode: "100644", Content: strings.Repeat("a", 483)},
					{Path: "second.rb", Mode: "100644", Content: strings.Repeat("a", 888)},
				})

				subTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "main.rb", Mode: "100644", Content: strings.Repeat("a", 500)},
					{Path: "formatter.rb", Mode: "100644", Content: strings.Repeat("a", 120)},
					{Path: "example", Mode: "040000", OID: subSubTree},
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "scripts", Mode: "040000", OID: subTree},
					gittest.TreeEntry{Path: "run.rb", Mode: "100644", Content: strings.Repeat("a", 55)},
					gittest.TreeEntry{
						Path: ".gitattributes",
						Mode: "100644",
						Content: "scripts/** linguist-documentation\n" +
							"scripts/formatter.rb -linguist-documentation",
					},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"Ruby": 175,
			},
		},
		{
			desc: "empty code files",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				emptyBlob := gittest.WriteBlob(t, cfg, repoPath, []byte{})
				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "README.md", Mode: "100644", Content: "Hello world!"},
					gittest.TreeEntry{Path: "index.html", Mode: "100644", OID: emptyBlob},
					gittest.TreeEntry{Path: "app.js", Mode: "100644", OID: emptyBlob},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{},
		},
		{
			desc: "preexisting cache",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "webpack.coffee", Mode: "100644", Content: strings.Repeat("a", 107)},
					gittest.TreeEntry{Path: "show_user.html", Mode: "100644", Content: strings.Repeat("a", 349)},
					gittest.TreeEntry{Path: "api.javascript", Mode: "100644", Content: strings.Repeat("a", 1014)},
					gittest.TreeEntry{Path: "application.rb", Mode: "100644", Content: strings.Repeat("a", 2943)},
				))
				repo := localrepo.NewTestRepo(t, cfg, repoProto)

				// We simply run the linguist once before so that it can already
				// write the cache.
				_, err := New(cfg, testhelper.NewLogger(t), catfileCache, repo).Stats(ctx, commitID)
				require.NoError(t, err)
				require.FileExists(t, filepath.Join(repoPath, languageStatsFilename))

				// Make sure it isn't able to generate stats from scratch
				require.NoError(t, os.RemoveAll(filepath.Join(repoPath, "objects", "pack")))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"CoffeeScript": 107,
				"HTML":         349,
				"JavaScript":   1014,
				"Ruby":         2943,
			},
		},
		{
			desc:  "preexisting cache with unknown name triggers recalculation",
			setup: setupStaleCacheTest("OldRubyName"),
			expectedStats: ByteCountPerLanguage{
				"Ruby": 2943,
			},
		},
		{
			desc:  "preexisting cache with known alias triggers recalculation",
			setup: setupStaleCacheTest("Mathematica"),
			expectedStats: ByteCountPerLanguage{
				"Ruby": 2943,
			},
		},
		{
			desc: "preexisting cache with .gitattributes modified",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "webpack.coffee", Mode: "100644", Content: strings.Repeat("a", 107)},
					gittest.TreeEntry{Path: "show_user.html", Mode: "100644", Content: strings.Repeat("a", 349)},
					gittest.TreeEntry{Path: "api.javascript", Mode: "100644", Content: strings.Repeat("a", 1014)},
					gittest.TreeEntry{Path: ".gitattributes", Mode: "100644", Content: "*.html linguist-vendored"},
				))
				repo := localrepo.NewTestRepo(t, cfg, repoProto)

				_, err := New(cfg, testhelper.NewLogger(t), catfileCache, repo).Stats(ctx, commitID)
				require.NoError(t, err)
				require.FileExists(t, filepath.Join(repoPath, languageStatsFilename))

				commitID = gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "webpack.coffee", Mode: "100644", Content: strings.Repeat("a", 107)},
					gittest.TreeEntry{Path: "show_user.html", Mode: "100644", Content: strings.Repeat("a", 349)},
					gittest.TreeEntry{Path: "api.javascript", Mode: "100644", Content: strings.Repeat("a", 1014)},
					gittest.TreeEntry{Path: ".gitattributes", Mode: "100644", Content: "*.coffee linguist-vendored"},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"HTML":       349,
				"JavaScript": 1014,
			},
		},
		{
			desc: "buggy behavior",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				includeTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "ffx_a.h", Mode: "100644", Content: "#include <stdio.h>\n"},
				})
				thirdPartyTree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
					{Path: "ffx_a.c", Mode: "100644", Content: "#include <include/ffx_a.h>\nstatic int something() {}"},
					{Path: "include", Mode: "040000", OID: includeTree},
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "thirdparty", Mode: "040000", OID: thirdPartyTree},
					gittest.TreeEntry{
						Path:    ".gitattributes",
						Mode:    "100644",
						Content: "*.h linguist-language=cpp\nthirdparty/* linguist-vendored",
					},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{},
		},
		{
			desc: "linguist-language with alias resolves to canonical name",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "main.c", Mode: "100644", Content: strings.Repeat("a", 100)},
					gittest.TreeEntry{Path: "network.msg", Mode: "100644", Content: strings.Repeat("a", 200)},
					gittest.TreeEntry{
						Path: ".gitattributes",
						Mode: "100644",
						Content: "*.msg linguist-language=omnetpp-msg\n" +
							"*.msg linguist-detectable\n",
					},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"C":           100,
				"OMNeT++ MSG": 200,
			},
		},
		{
			desc: "linguist-language with canonical name",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "script.foo", Mode: "100644", Content: "puts 'hello'"},
					gittest.TreeEntry{
						Path:    ".gitattributes",
						Mode:    "100644",
						Content: "*.foo linguist-language=Ruby",
					},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"Ruby": 12,
			},
		},
		{
			desc: "corrupted cache",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "webpack.coffee", Mode: "100644", Content: strings.Repeat("a", 107)},
					gittest.TreeEntry{Path: "show_user.html", Mode: "100644", Content: strings.Repeat("a", 349)},
					gittest.TreeEntry{Path: "api.javascript", Mode: "100644", Content: strings.Repeat("a", 1014)},
					gittest.TreeEntry{Path: "application.rb", Mode: "100644", Content: strings.Repeat("a", 2943)},
				))

				require.NoError(t, os.WriteFile(filepath.Join(repoPath, languageStatsFilename), []byte("garbage"), mode.File))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"CoffeeScript": 107,
				"HTML":         349,
				"JavaScript":   1014,
				"Ruby":         2943,
			},
		},
		{
			desc: "old cache",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				repo := localrepo.NewTestRepo(t, cfg, repoProto)

				oldCommitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "main.rb", Content: "require 'fileutils'", Mode: "100644"},
				))
				newCommitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(oldCommitID), gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "main.go", Content: "package main", Mode: "100644"},
				))

				// Precreate the cache with the old commit. This ensures that
				// linguist knows to update the cache.
				stats, err := New(cfg, testhelper.NewLogger(t), catfileCache, repo).Stats(ctx, oldCommitID)
				require.NoError(t, err)
				require.FileExists(t, filepath.Join(repoPath, languageStatsFilename))
				require.Equal(t, ByteCountPerLanguage{
					"Ruby": 19,
				}, stats)

				return repoProto, repoPath, newCommitID
			},
			expectedStats: ByteCountPerLanguage{
				"Go": 12,
			},
		},
		{
			desc: "preexisting cache with path changed into a submodule",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				repo := localrepo.NewTestRepo(t, cfg, repoProto)

				commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "main.go", Mode: "100644", Content: "package main"},
					gittest.TreeEntry{Path: "theme", Mode: "100644", Content: "theme contents"},
				))

				// Populate the cache so the next run takes the incremental path.
				_, err := New(cfg, testhelper.NewLogger(t), catfileCache, repo).Stats(ctx, commitID)
				require.NoError(t, err)
				require.FileExists(t, filepath.Join(repoPath, languageStatsFilename))

				// The gitlink points at a commit absent from this repository.
				bogusSubmodule := git.ObjectID(strings.Repeat("a", gittest.DefaultObjectHash.EncodedLen()))

				commitID = gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "main.go", Mode: "100644", Content: "package main"},
					gittest.TreeEntry{Path: "theme", Mode: "160000", OID: bogusSubmodule},
				))

				return repoProto, repoPath, commitID
			},
			expectedStats: ByteCountPerLanguage{
				"Go": 12,
			},
		},
		{
			desc: "stale cached commit no longer exists",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})
				repo := localrepo.NewTestRepo(t, cfg, repoProto)

				commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "main.go", Content: "package main", Mode: "100644"},
				))

				// Build a syntactically valid but non-existent object hash.
				bogusObjectID := git.ObjectID(strings.Repeat("a", gittest.DefaultObjectHash.EncodedLen()))
				exists, err := repo.HasRevision(ctx, git.Revision(bogusObjectID.String()+"^{commit}"))
				require.NoError(t, err)
				require.False(t, exists)

				// Save a cache whose CommitID references the non-existent commit.
				stats, err := initLanguageStats(ctx, repo)
				require.NoError(t, err)
				require.NoError(t, stats.save(ctx, repo, bogusObjectID))

				return repoProto, repoPath, commit
			},
			expectedStats: ByteCountPerLanguage{
				"Go": 12,
			},
		},
		{
			desc: "missing repository",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoPath := filepath.Join(testhelper.TempDir(t), "nonexistent")
				repoProto := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "nonexistent"}

				return repoProto, repoPath, git.ObjectID("b1bb1d1b0b1d1b00")
			},
			expectedErr: storage.ErrRepositoryNotFound.Error(),
		},
		{
			desc: "missing commit",
			setup: func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID) {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
				})

				return repoProto, repoPath, git.ObjectID("b1bb1d1b0b1d1b00")
			},
			expectedErr: "linguist",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			repoProto, repoPath, objectID := tc.setup(t)

			repo := localrepo.NewTestRepo(t, cfg, repoProto)
			linguist := New(cfg, testhelper.NewLogger(t), catfileCache, repo)
			stats, err := linguist.Stats(ctx, objectID)
			if tc.expectedErr == "" {
				require.NoError(t, err)
				require.Equal(t, tc.expectedStats, stats)
				require.FileExists(t, filepath.Join(repoPath, languageStatsFilename))
			} else {
				require.Contains(t, err.Error(), tc.expectedErr)
			}
		})
	}
}

func TestInstance_Stats_failureGitattributes(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)
	locator := config.NewLocator(cfg)
	logger := testhelper.NewLogger(t)

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	gitCmdFactory := gittest.NewInterceptingCommandFactory(t, ctx, cfg, func(execEnv gitcmd.ExecutionEnvironment) string {
		return fmt.Sprintf(`#!/usr/bin/env bash
		if [[ ! "$@" =~ "check-attr" ]]; then
			exec %q "$@"
		fi
		exit 1'
		`, execEnv.BinaryPath)
	})

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "main.rb", Mode: "100644", Content: strings.Repeat("a", 107)},
		gittest.TreeEntry{Path: ".gitattributes", Mode: "100644", Content: "*.rb -linguist-vendored"},
	))

	repo := localrepo.New(logger, locator, gitCmdFactory, catfileCache, repoProto)

	linguist := New(cfg, logger, catfileCache, repo)
	_, err := linguist.Stats(ctx, commitID)

	expectedErr := `linguist object iterator: ls-tree skip: new file instance: checking attribute:`
	require.ErrorContains(t, err, expectedErr)
}

func TestColor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		language      string
		expectedColor string
	}{
		{language: "Go", expectedColor: "#00ADD8"},
		{language: "Ruby", expectedColor: "#701516"},
		{language: "HTML", expectedColor: "#e34c26"},
		{language: "Markdown", expectedColor: "#083fa1"},
		{language: "Javascript", expectedColor: "#75712c"},
		{language: "SSH Config", expectedColor: "#d1dbe0"},    // grouped into INI by go-enry
		{language: "Wozzle Wuzzle", expectedColor: "#3adbcf"}, // non-existing language
	} {
		t.Run(tc.language, func(t *testing.T) {
			require.Equal(t, tc.expectedColor, Color(tc.language), "color value for '%v'", tc.language)
		})
	}
}

func TestResolveLanguage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc         string
		language     string
		expectedName string
		expectedID   int
		expectErr    bool
	}{
		{
			desc:         "canonical name",
			language:     "Go",
			expectedName: "Go",
			expectedID:   132,
		},
		{
			desc:         "alias resolves to canonical",
			language:     "Mathematica",
			expectedName: "Wolfram Language",
			expectedID:   224,
		},
		{
			desc:         "unknown language returns error",
			language:     "totallyinvalid",
			expectedName: "totallyinvalid",
			expectedID:   -1,
			expectErr:    true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			name, id, err := ResolveLanguage(tc.language)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.expectedName, name)
			require.Equal(t, tc.expectedID, id)
		})
	}
}

func TestHasStaleLanguageNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		totals   ByteCountPerLanguage
		expected bool
	}{
		{
			desc:     "all canonical",
			totals:   ByteCountPerLanguage{"Go": 100},
			expected: false,
		},
		{
			desc:     "known alias is stale",
			totals:   ByteCountPerLanguage{"Mathematica": 100},
			expected: true,
		},
		{
			desc:     "unknown name is stale",
			totals:   ByteCountPerLanguage{"totallyinvalid": 50},
			expected: true,
		},
		{
			desc:     "mix of canonical and unknown is stale",
			totals:   ByteCountPerLanguage{"Go": 100, "totallyinvalid": 50},
			expected: true,
		},
		{
			desc:     "empty map",
			totals:   ByteCountPerLanguage{},
			expected: false,
		},
		{
			desc:     "nil map",
			totals:   nil,
			expected: false,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, hasStaleLanguageNames(tc.totals))
		})
	}
}

func BenchmarkInstance_Stats(b *testing.B) {
	cfg := testcfg.Build(b)
	ctx := testhelper.Context(b)

	catfileCache := catfile.NewCache(cfg)
	b.Cleanup(catfileCache.Stop)

	repoProto, repoPath := gittest.CreateRepository(b, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		Seed:                   "benchmark.git",
	})
	repo := localrepo.NewTestRepo(b, cfg, repoProto)

	linguist := New(cfg, testhelper.NewLogger(b), catfileCache, repo)

	var scratchStat ByteCountPerLanguage
	var incStats ByteCountPerLanguage

	b.Run("from scratch", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			require.NoError(b, os.RemoveAll(filepath.Join(repoPath, languageStatsFilename)))
			b.StartTimer()

			var err error
			scratchStat, err = linguist.Stats(ctx, "f5dfdd0057cd6bffc6259a5c8533dde5bf6a9d37")
			require.NoError(b, err)
		}
	})

	b.Run("incremental", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			require.NoError(b, os.RemoveAll(filepath.Join(repoPath, languageStatsFilename)))
			// a commit about 3 months older than the next
			_, err := linguist.Stats(ctx, "3c813b292d25a9b2ffda70e7f609f623bfc0cb37")
			require.NoError(b, err)
			b.StartTimer()

			incStats, err = linguist.Stats(ctx, "f5dfdd0057cd6bffc6259a5c8533dde5bf6a9d37")
			require.NoError(b, err)
		}
	})

	require.Equal(b, scratchStat, incStats)
}
