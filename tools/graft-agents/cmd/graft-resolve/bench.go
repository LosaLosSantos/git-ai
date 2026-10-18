package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/bench"
)

// runBench runs the benchmark subcommands:
//
//	graft-resolve bench mine --corpus FILE --clones DIR --manifest OUT [--max-cases-per-repo N]
//	graft-resolve bench run --config FILE --manifest FILE --clones DIR [--sample N] [--seed S]
//	                        [--label L] [--json-out FILE] [--md-out FILE]
func runBench(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: graft-resolve bench mine|run [flags]")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "mine":
		return benchMine(ctx, args[1:], stdout, stderr)
	case "run":
		return benchRun(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "graft-resolve bench: unknown subcommand %q, want mine or run\n", args[0])
	return 2
}

func benchMine(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-resolve bench mine", flag.ContinueOnError)
	fs.SetOutput(stderr)
	corpusPath := fs.String("corpus", "", "corpus file (JSON)")
	clones := fs.String("clones", "", "directory of the clones of the corpus repositories")
	manifestPath := fs.String("manifest", "", "manifest to write")
	maxCases := fs.Int("max-cases-per-repo", 0, "bound on the cases kept per repository (0: none)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *corpusPath == "" || *clones == "" || *manifestPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "graft-resolve bench mine: --corpus, --clones and --manifest are required")
		return 2
	}

	corpus, err := bench.LoadCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench mine: %v\n", err)
		return 1
	}

	manifest := bench.Manifest{Version: bench.ManifestVersion, Stats: map[string]bench.MineStats{}}
	for _, repo := range corpus.Repos {
		dir, err := bench.EnsureClone(ctx, *clones, repo)
		if err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench mine: %s: %v\n", repo.Name, err)
			return 1
		}
		cases, stats, err := bench.Mine(ctx, repo, dir, bench.MineOptions{MaxCases: *maxCases})
		if err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench mine: %s: %v\n", repo.Name, err)
			return 1
		}
		manifest.Cases = append(manifest.Cases, cases...)
		manifest.Stats[repo.Name] = stats
		fmt.Fprintf(stdout, "%s: %d merges, %d conflicting, %d conflicting files, %d kept\n",
			repo.Name, stats.Merges, stats.ConflictingMerges, stats.ConflictingFiles, stats.Kept)
	}

	if err := bench.SaveManifest(*manifestPath, manifest); err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench mine: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%d cases in %s\n", len(manifest.Cases), *manifestPath)
	return 0
}

func benchRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-resolve bench run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "resolver configuration (TOML), as in production")
	manifestPath := fs.String("manifest", "", "manifest written by bench mine")
	clones := fs.String("clones", "", "directory of the clones of the corpus repositories")
	sample := fs.Int("sample", 0, "number of cases, stratified by family (0: every case)")
	seed := fs.Int64("seed", 1, "seed of the sample")
	label := fs.String("label", "", "name of the run in the report")
	jsonOut := fs.String("json-out", "", "report as JSON")
	mdOut := fs.String("md-out", "", "report as Markdown")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifestPath == "" || *clones == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "graft-resolve bench run: --manifest and --clones are required")
		return 2
	}

	res, err := loadResolver(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
		return 1
	}
	manifest, err := bench.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
		return 1
	}

	cases := bench.Sample(manifest.Cases, *sample, *seed)
	runner := bench.Runner{
		Resolver: res,
		Clone:    func(repo string) string { return bench.CloneDir(*clones, repo) },
		Progress: func(done, total int, r bench.Result) {
			fmt.Fprintf(stderr, "[%d/%d] %s %s %s: %s %s\n", done, total, r.Family, r.Repo, r.Path, r.Outcome, r.Reason)
		},
	}
	results, runErr := runner.Run(ctx, cases)
	report := bench.NewReport(*label, results)

	if *jsonOut != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, append(data, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
			return 1
		}
	}
	out := stdout
	if *mdOut != "" {
		f, err := os.Create(*mdOut)
		if err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
			return 1
		}
		defer f.Close()
		out = io.MultiWriter(stdout, f)
	}
	if err := report.WriteMarkdown(out); err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
		return 1
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "graft-resolve bench run: stopped after %d of %d cases: %v\n", len(results), len(cases), runErr)
		return 1
	}
	return 0
}
