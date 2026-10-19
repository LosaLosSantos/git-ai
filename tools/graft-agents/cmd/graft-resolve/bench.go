package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/bench"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// runBench runs the benchmark subcommands:
//
//	graft-resolve bench mine --corpus FILE --clones DIR --manifest OUT [--max-cases-per-repo N]
//	graft-resolve bench run ... --max-usd USD [--pricing openrouter|fixed]
//	                        [--input-usd-per-million P --output-usd-per-million P]
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
	case "rejudge":
		return benchRejudge(ctx, args[1:], stdout, stderr)
	case "syntax":
		return benchSyntax(ctx, args[1:], stdout, stderr)
	case "filter":
		return benchFilter(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "graft-resolve bench: unknown subcommand %q, want mine, run, rejudge, syntax or filter\n", args[0])
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

	if !syntax.Available {
		fmt.Fprintln(stderr, "graft-resolve bench mine: this graft-resolve was built without cgo, and mining checks the syntax of the human resolutions; build it with CGO_ENABLED=1 and a C compiler")
		return 1
	}
	corpus, err := bench.LoadCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench mine: %v\n", err)
		return 1
	}

	// Each repository's cases are kept in a part of their own as soon as it is mined, so a run
	// that is stopped resumes with the next repository.
	partsDir := *manifestPath + ".parts"
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench mine: %v\n", err)
		return 1
	}

	manifest := bench.Manifest{Version: bench.ManifestVersion, Stats: map[string]bench.MineStats{}}
	for _, repo := range corpus.Repos {
		partPath := filepath.Join(partsDir, strings.ReplaceAll(repo.Name, "/", "__")+"@"+repo.Commit+".json")
		part, err := bench.LoadManifest(partPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(stderr, "graft-resolve bench mine: %s: %v\n", repo.Name, err)
				return 1
			}
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
			part = bench.Manifest{Version: bench.ManifestVersion, Cases: cases, Stats: map[string]bench.MineStats{repo.Name: stats}}
			if err := bench.SaveManifest(partPath, part); err != nil {
				fmt.Fprintf(stderr, "graft-resolve bench mine: %v\n", err)
				return 1
			}
		}
		stats := part.Stats[repo.Name]
		manifest.Cases = append(manifest.Cases, part.Cases...)
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
	answersDir := fs.String("answers-dir", "", "keep every accepted resolution here, for audit and bench rejudge (local: third-party code)")
	mdOut := fs.String("md-out", "", "report as Markdown")
	maxUSD := fs.Float64("max-usd", 0, "hard cap on the AI spend of the run in USD (required when the chain has an llm strategy)")
	pricingSource := fs.String("pricing", "openrouter", "where the model's price comes from: openrouter (read at run time) or fixed (the two price flags)")
	inputPrice := fs.Float64("input-usd-per-million", 0, "input price, with --pricing fixed")
	outputPrice := fs.Float64("output-usd-per-million", 0, "output price, reasoning included, with --pricing fixed")
	openRouterAPI := fs.String("openrouter-api", bench.DefaultOpenRouterAPI, "OpenRouter API base, for --pricing openrouter")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifestPath == "" || *clones == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "graft-resolve bench run: --manifest and --clones are required")
		return 2
	}

	if !syntax.Available {
		fmt.Fprintln(stderr, "graft-resolve bench run: this graft-resolve was built without cgo, so the syntax check would reject every answer; build it with CGO_ENABLED=1")
		return 1
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

	// A chain with a model runs under a hard cap: every call reserves its worst case first
	// (resolve.Budget), and the calls that would pass the cap are not made.
	var budget *resolve.Budget
	var pricing *bench.Pricing
	if models := res.LLMModels(); len(models) > 0 {
		if *maxUSD <= 0 {
			fmt.Fprintln(stderr, "graft-resolve bench run: the chain calls a model, so --max-usd is required")
			return 2
		}
		var prices resolve.Prices
		switch *pricingSource {
		case "openrouter":
			distinct := map[string]bool{}
			for _, m := range models {
				distinct[m] = true
			}
			if len(distinct) != 1 {
				fmt.Fprintf(stderr, "graft-resolve bench run: --pricing openrouter prices one model, the chain has %v\n", models)
				return 2
			}
			var model string
			for m := range distinct {
				model = m
			}
			p, err := bench.OpenRouterPricing(ctx, &http.Client{Timeout: 30 * time.Second}, *openRouterAPI, model, res.LLMMaxPromptTokens())
			if err != nil {
				fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
				return 1
			}
			pricing, prices = &p, p.Prices()
		case "fixed":
			prices = resolve.Prices{InputPerMillion: *inputPrice, OutputPerMillion: *outputPrice}
		default:
			fmt.Fprintf(stderr, "graft-resolve bench run: unknown --pricing %q, want openrouter or fixed\n", *pricingSource)
			return 2
		}
		if budget, err = resolve.NewBudget(*maxUSD, prices, ""); err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
			return 1
		}
		if _, err := res.UseBudget(budget); err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "cap $%.2f; $%g per million input tokens, $%g per million output tokens, $%g per request\n",
			*maxUSD, prices.InputPerMillion, prices.OutputPerMillion, prices.PerRequest)
	}

	cases := bench.Sample(manifest.Cases, *sample, *seed)
	if *answersDir != "" {
		if err := os.MkdirAll(*answersDir, 0o700); err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
			return 1
		}
	}
	runner := bench.Runner{
		Resolver:   res,
		AnswersDir: *answersDir,
		Clone:      func(repo string) string { return bench.CloneDir(*clones, repo) },
		Progress: func(done, total int, r bench.Result) {
			fmt.Fprintf(stderr, "[%d/%d] %s %s %s: %s %s\n", done, total, r.Family, r.Repo, r.Path, r.Outcome, r.Reason)
		},
	}
	results, runErr := runner.Run(ctx, cases)
	report := bench.NewReport(*label, results)
	if budget != nil {
		snap, err := budget.Snapshot()
		if err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
			return 1
		}
		report.SpendUSD, report.MaxUSD, report.Pricing = snap.SpentUSD, *maxUSD, pricing
	}

	if err := writeReport(report, *jsonOut, *mdOut, stdout); err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench run: %v\n", err)
		return 1
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "graft-resolve bench run: stopped after %d of %d cases: %v\n", len(results), len(cases), runErr)
		return 1
	}
	return 0
}

// benchRejudge judges the stored answers of a run again, with no model call:
//
//	graft-resolve bench rejudge --report-in FILE --manifest FILE --clones DIR --answers-dir DIR
//	                            [--json-out FILE] [--md-out FILE]
//
// The report keeps everything of the run (label, spend, pricing, the results that were not
// accepted) and recomputes the outcome of every accepted resolution.
func benchRejudge(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-resolve bench rejudge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reportIn := fs.String("report-in", "", "JSON report of the run")
	manifestPath := fs.String("manifest", "", "manifest of the run")
	clones := fs.String("clones", "", "directory of the clones of the corpus repositories")
	answersDir := fs.String("answers-dir", "", "answers kept by the run (bench run --answers-dir)")
	jsonOut := fs.String("json-out", "", "report as JSON")
	mdOut := fs.String("md-out", "", "report as Markdown")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *reportIn == "" || *manifestPath == "" || *clones == "" || *answersDir == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "graft-resolve bench rejudge: --report-in, --manifest, --clones and --answers-dir are required")
		return 2
	}
	raw, err := os.ReadFile(*reportIn)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench rejudge: %v\n", err)
		return 1
	}
	var old bench.Report
	if err := json.Unmarshal(raw, &old); err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench rejudge: %s: %v\n", *reportIn, err)
		return 1
	}
	manifest, err := bench.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench rejudge: %v\n", err)
		return 1
	}
	results, err := bench.Rejudge(ctx, old.Results, manifest.Cases, func(repo string) string { return bench.CloneDir(*clones, repo) }, *answersDir)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench rejudge: %v\n", err)
		return 1
	}
	report := bench.NewReport(old.Label, results)
	report.SpendUSD, report.MaxUSD, report.Pricing = old.SpendUSD, old.MaxUSD, old.Pricing
	if err := writeReport(report, *jsonOut, *mdOut, stdout); err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench rejudge: %v\n", err)
		return 1
	}
	return 0
}

// writeReport writes report as JSON to jsonOut and as Markdown to stdout and mdOut, when set.
func writeReport(report bench.Report, jsonOut, mdOut string, stdout io.Writer) error {
	if jsonOut != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonOut, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	out := stdout
	if mdOut != "" {
		f, err := os.Create(mdOut)
		if err != nil {
			return err
		}
		defer f.Close()
		out = io.MultiWriter(stdout, f)
	}
	return report.WriteMarkdown(out)
}

// benchSyntax runs the syntax check on every human resolution of a manifest, with no model call:
//
//	graft-resolve bench syntax --manifest FILE --clones DIR [--json-out FILE]
//
// It prints the checked and failed counts per language and every failure.
func benchSyntax(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-resolve bench syntax", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifestPath := fs.String("manifest", "", "manifest written by bench mine")
	clones := fs.String("clones", "", "directory of the clones of the corpus repositories")
	jsonOut := fs.String("json-out", "", "counts and failures as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifestPath == "" || *clones == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "graft-resolve bench syntax: --manifest and --clones are required")
		return 2
	}
	if !syntax.Available {
		fmt.Fprintln(stderr, "graft-resolve bench syntax: this graft-resolve was built without cgo; build it with CGO_ENABLED=1")
		return 1
	}
	manifest, err := bench.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench syntax: %v\n", err)
		return 1
	}
	stats, findings, err := bench.HumanSyntax(ctx, manifest.Cases, func(repo string) string { return bench.CloneDir(*clones, repo) })
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench syntax: %v\n", err)
		return 1
	}
	if *jsonOut != "" {
		data, err := json.MarshalIndent(map[string]any{"languages": stats, "failures": findings}, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, append(data, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintf(stderr, "graft-resolve bench syntax: %v\n", err)
			return 1
		}
	}
	langs := make([]string, 0, len(stats))
	for l := range stats {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, l := range langs {
		s := stats[l]
		fmt.Fprintf(stdout, "%s: %d with both sides parsing, %d of them fail (%.1f%%); %d with a side the grammar does not parse (no verdict)\n",
			l, s.Checked, s.Failed, 100*float64(s.Failed)/float64(max(1, s.Checked)), s.Unchecked)
	}
	for _, f := range findings {
		if f.Unchecked {
			fmt.Fprintf(stdout, "no verdict: %s %s %s %s: %s\n", f.ID, f.Repo, f.Merge, f.Path, f.Reason)
			continue
		}
		fmt.Fprintf(stdout, "fails: %s %s %s %s (conflict markers: %v): %s\n", f.ID, f.Repo, f.Merge, f.Path, f.Markers, f.Reason)
	}
	return 0
}

// benchFilter leaves out the cases of a manifest whose human resolution is not a valid reference
// (bench mine applies the same rule to what it mines):
//
//	graft-resolve bench filter --manifest IN --clones DIR --manifest-out OUT
func benchFilter(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-resolve bench filter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifestPath := fs.String("manifest", "", "manifest to filter")
	clones := fs.String("clones", "", "directory of the clones of the corpus repositories")
	outPath := fs.String("manifest-out", "", "filtered manifest to write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifestPath == "" || *clones == "" || *outPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "graft-resolve bench filter: --manifest, --clones and --manifest-out are required")
		return 2
	}
	manifest, err := bench.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench filter: %v\n", err)
		return 1
	}
	kept, stats, err := bench.Filter(ctx, manifest.Cases, func(repo string) string { return bench.CloneDir(*clones, repo) })
	if err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench filter: %v\n", err)
		return 1
	}
	for repo, s := range manifest.Stats {
		for reason, n := range stats {
			if strings.HasPrefix(reason, repo+": ") {
				s.InvalidReference += n
				s.Kept -= n
			}
		}
		manifest.Stats[repo] = s
	}
	manifest.Cases = kept
	if err := bench.SaveManifest(*outPath, manifest); err != nil {
		fmt.Fprintf(stderr, "graft-resolve bench filter: %v\n", err)
		return 1
	}
	reasons := make([]string, 0, len(stats))
	for r := range stats {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	left := 0
	for _, r := range reasons {
		fmt.Fprintf(stdout, "left out %d: %s\n", stats[r], r)
		left += stats[r]
	}
	fmt.Fprintf(stdout, "%d of %d cases kept in %s\n", len(kept), len(kept)+left, *outPath)
	return 0
}
