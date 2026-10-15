package resolve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/protocol"
)

// maxCommandOutput bounds the result of a command run: the size of {out} or of
// the tool's stdout.
const maxCommandOutput = 16 << 20

// command runs an external merge tool. The argv may contain {base}, {ours},
// {theirs} and {out}. The tool reads the three versions, which are regular
// files in a fresh temporary directory (an absent base is an empty file). The
// result comes from one of two places, chosen by the output option:
//
//   - "file" (default): the tool writes its result to {out}. {out} does not
//     exist before the run; a zero exit status without a regular {out} is an
//     error, so a tool that forgot to write its result cannot land an empty file.
//   - "stdout": the tool writes its result to stdout. An empty stdout is an
//     error; an intentionally empty file must use the file mode.
//
// The tool runs only when ours and theirs both exist as regular files with the
// same mode (modify-delete and symlinks are left to other strategies). The
// env option lists extra environment variables to pass through.
type command struct {
	argv    []string
	timeout time.Duration
	env     []string
	stdout  bool
}

func newCommand(spec StrategySpec) (Strategy, error) {
	if err := allowOnly(spec, "command", "timeout", "output", "env"); err != nil {
		return nil, err
	}
	argv, err := optStringList(spec, "command")
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("strategy %q: command needs a non-empty command", spec.Name)
	}
	timeout, err := optDuration(spec, "timeout", DefaultCommandTimeout)
	if err != nil {
		return nil, err
	}
	output, err := optString(spec, "output")
	if err != nil {
		return nil, err
	}
	var stdout bool
	switch output {
	case "", "file":
	case "stdout":
		stdout = true
	default:
		return nil, fmt.Errorf("strategy %q: output must be \"file\" or \"stdout\", got %q", spec.Name, output)
	}
	env, err := optStringList(spec, "env")
	if err != nil {
		return nil, err
	}
	if err := validateEnvNames(env); err != nil {
		return nil, fmt.Errorf("strategy %q: %w", spec.Name, err)
	}
	return command{argv: argv, timeout: timeout, env: env, stdout: stdout}, nil
}

func (c command) Resolve(ctx context.Context, f File, _ protocol.Request) (Resolution, bool, error) {
	if !regularFile(f.Ours) || !regularFile(f.Theirs) || f.Ours.Mode != f.Theirs.Mode {
		return Resolution{}, false, nil
	}
	if f.Base != nil && f.Base.Mode == protocol.ModeSymlink {
		return Resolution{}, false, nil
	}

	dir, err := os.MkdirTemp("", "graft-resolve-command-")
	if err != nil {
		return Resolution{}, false, fmt.Errorf("creating work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	paths := map[string]string{
		"{base}":   filepath.Join(dir, "base"),
		"{ours}":   filepath.Join(dir, "ours"),
		"{theirs}": filepath.Join(dir, "theirs"),
		"{out}":    filepath.Join(dir, "out"),
	}
	inputs := map[string][]byte{"{ours}": f.Ours.Content, "{theirs}": f.Theirs.Content}
	if f.Base != nil {
		inputs["{base}"] = f.Base.Content
	} else {
		inputs["{base}"] = nil
	}
	for placeholder, content := range inputs {
		if err := os.WriteFile(paths[placeholder], content, 0o600); err != nil {
			return Resolution{}, false, fmt.Errorf("writing command input: %w", err)
		}
	}

	spec := execSpec{dir: dir, argv: substitute(c.argv, paths), timeout: c.timeout, env: c.env}
	if c.stdout {
		spec.stdoutLimit = maxCommandOutput
	}
	stdout, stderr, err := execute(ctx, spec)
	if err != nil {
		return Resolution{}, false, withStderr(fmt.Errorf("%s: %w", c.argv[0], err), stderr)
	}

	var content []byte
	if c.stdout {
		content, err = stdoutResult(stdout)
	} else {
		content, err = fileResult(paths["{out}"])
	}
	if err != nil {
		return Resolution{}, false, err
	}
	return Resolution{Action: protocol.ActionWrite, Mode: f.Ours.Mode, Content: content}, true, nil
}

// stdoutResult validates the captured stdout of a stdout-mode run.
func stdoutResult(stdout []byte) ([]byte, error) {
	if int64(len(stdout)) > maxCommandOutput {
		return nil, fmt.Errorf("command output exceeds the %d byte limit", maxCommandOutput)
	}
	if len(stdout) == 0 {
		return nil, errors.New("command wrote no output to stdout")
	}
	return stdout, nil
}

// fileResult reads the {out} file written by a file-mode run.
func fileResult(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("command exited 0 without writing {out}")
	case err != nil:
		return nil, fmt.Errorf("reading command output: %w", err)
	case !fi.Mode().IsRegular():
		return nil, errors.New("command output {out} is not a regular file")
	case fi.Size() > maxCommandOutput:
		return nil, fmt.Errorf("command output of %d bytes exceeds the %d byte limit", fi.Size(), maxCommandOutput)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("reading command output: %w", err)
	}
	defer func() { _ = f.Close() }()
	// The tool may still be writing if it left a background process behind, so
	// the read is bounded as well as the stat.
	content, err := io.ReadAll(io.LimitReader(f, maxCommandOutput+1))
	if err != nil {
		return nil, fmt.Errorf("reading command output: %w", err)
	}
	if int64(len(content)) > maxCommandOutput {
		return nil, fmt.Errorf("command output exceeds the %d byte limit", maxCommandOutput)
	}
	return content, nil
}
