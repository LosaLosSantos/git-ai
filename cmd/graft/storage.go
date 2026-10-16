package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/sdk"
)

// storageSynopsis is the command line of graft storage.
const storageSynopsis = "usage: graft storage import|state|verify [-repo R] [--connectivity] [--json]"

// storageUsage lists the subcommands of graft storage.
const storageUsage = "storage requires a subcommand: import, state or verify (" + storageSynopsis + ")"

// storageFlags holds the flags of graft storage. Each subcommand reads only
// some of them, as its help text says.
type storageFlags struct {
	repo         string
	connectivity bool
}

// storageCommand returns the storage command, the operator's view of the
// storage core (import, state and verify of a repository).
func storageCommand() command {
	f := &storageFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\nflags:\n", storageSynopsis)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.repo, "repo", "", "repository path, used without a workspace together with -address and -storage")
			fs.BoolVar(&f.connectivity, "connectivity", false, "verify: also check the connectivity of the local repository (git fsck --connectivity-only) and that the object storage holds every object of the database references")
			fs.BoolVar(&o.jsonOut, "json", false, "import, state, verify: print the result as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdStorage(ctx, out, o, f, args)
		},
	}
}

// cmdStorage dispatches a storage subcommand. Arguments are checked before the
// server is contacted.
func cmdStorage(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", storageUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "import":
		return storageImport(ctx, out, o, f, rest)
	case "state":
		return storageState(ctx, out, o, f, rest)
	case "verify":
		return storageVerify(ctx, out, o, f, rest)
	}
	return usagef("unknown storage subcommand %q: %s", sub, storageUsage)
}

func storageImport(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage import takes flags only: import [-repo R] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageImport(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	if res.References == 0 {
		// A history import: a first import always records HEAD.
		fmt.Fprintf(out, "already imported at changeset %d: imported %d changelog entries and %d reviews written since\n",
			res.Sequence, res.ChangelogEntries, res.Reviews)
		return nil
	}
	fmt.Fprintf(out, "imported repository: changeset %d, %d references, %d changelog entries, %d reviews, pack %d bytes\n",
		res.Sequence, res.References, res.ChangelogEntries, res.Reviews, res.PackBytes)
	return nil
}

func storageState(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage state takes flags only: state [-repo R] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	st, err := repo.StorageState(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, st)
	}
	// The node name comes from the configuration of the server, so it is shown escaped.
	fmt.Fprintf(out, "mode\t%s\n", displayText(st.Mode))
	fmt.Fprintf(out, "tracked\t%t\n", st.Tracked)
	fmt.Fprintf(out, "repository_id\t%d\n", st.RepositoryID)
	fmt.Fprintf(out, "head_sequence\t%d\n", st.HeadSequence)
	fmt.Fprintf(out, "applied_sequence\t%d\n", st.AppliedSequence)
	fmt.Fprintf(out, "node_id\t%s\n", displayText(st.NodeID))
	return nil
}

func storageVerify(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage verify takes flags only: verify [-repo R] [--connectivity] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageVerify(ctx, f.connectivity)
	if err != nil {
		return mapError(err)
	}

	if o.jsonOut {
		if err := writeStorageJSON(out, storageVerifyView(res)); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "head sequence %d\n", res.HeadSequence)
		// Reference names come from the database and the repository: displayText
		// keeps control characters in them from forging lines of the output.
		for _, d := range res.Divergences {
			fmt.Fprintf(out, "divergence\t%s\tlocal=%s\tdatabase=%s\n",
				displayText(d.Reference), storageTargetText(d.LocalTarget), storageTargetText(d.DatabaseTarget))
		}
		if f.connectivity {
			if res.ConnectivityOK {
				fmt.Fprintln(out, "connectivity ok")
			} else {
				fmt.Fprintf(out, "connectivity failed: %s\n", displayText(res.ConnectivityError))
			}
		}
		if len(res.Divergences) == 0 && (!f.connectivity || res.ConnectivityOK) {
			fmt.Fprintln(out, "consistent")
		}
	}

	if len(res.Divergences) > 0 {
		return fmt.Errorf("storage verify: %d reference(s) diverge from the database", len(res.Divergences))
	}
	if f.connectivity && !res.ConnectivityOK {
		return errors.New("storage verify: connectivity check failed")
	}
	return nil
}

// openStorage returns the repository handle of a storage command. The server
// comes from the workspace, or without a workspace from -address, -storage and
// -repo, exactly as for graft review.
func openStorage(ctx context.Context, o *options, f *storageFlags) (*sdk.Repo, error) {
	t, _, err := reviewTarget(o, &reviewFlags{repo: f.repo})
	if err != nil {
		return nil, err
	}
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return nil, err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return nil, errors.New("storage: the backend cannot reach the storage core")
	}
	return sb.repo, nil
}

// storageTargetText shows a reference value, with the empty value as absent.
func storageTargetText(t string) string {
	if t == "" {
		return "absent"
	}
	return displayText(t)
}

// storageDivergenceJSON is a divergence in the JSON output of verify.
type storageDivergenceJSON struct {
	sdk.StorageDivergence
	// ReferenceBytes is the exact reference name (base64) when it is not valid
	// UTF-8: JSON strings cannot carry such bytes, so "reference" holds them
	// replaced by U+FFFD.
	ReferenceBytes []byte `json:"reference_bytes,omitempty"`
}

// storageVerifyJSON is the JSON output of verify: the SDK result, with the
// divergences extended by storageDivergenceJSON.
type storageVerifyJSON struct {
	*sdk.StorageVerify
	Divergences []storageDivergenceJSON `json:"divergences"`
}

// storageVerifyView returns the JSON output of a verify result.
func storageVerifyView(res *sdk.StorageVerify) storageVerifyJSON {
	view := storageVerifyJSON{StorageVerify: res}
	if res.Divergences != nil {
		view.Divergences = make([]storageDivergenceJSON, 0, len(res.Divergences))
	}
	for _, d := range res.Divergences {
		entry := storageDivergenceJSON{StorageDivergence: d}
		if !utf8.ValidString(d.Reference) {
			entry.ReferenceBytes = []byte(d.Reference)
		}
		view.Divergences = append(view.Divergences, entry)
	}
	return view
}

// writeStorageJSON prints v as one line of JSON.
func writeStorageJSON(out io.Writer, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", line)
	return err
}
