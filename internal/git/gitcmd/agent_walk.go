package gitcmd

import (
	"bufio"
	"context"
	"io"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/agentrefs"
	"github.com/feder-cr/git-ai/internal/graft/identity"
)

// An agent does not see the references under refs/graft/ (agentrefs). The requests of agents are
// checked for revisions that name them, but git's pseudo references reach every reference without
// naming one: --all, and --glob with a pattern such as refs/*. So every command of an agent that
// walks them (scWalksRefs), whatever RPC or request field asked for the walk, gets
// --exclude-hidden=uploadpack before each --all and --glob, in its arguments and on its stdin, with
// uploadpack.hideRefs set to refs/graft/: git then leaves the hidden references out of each walk.
// --exclude-hidden applies to the next pseudo reference only, hence one before each. --branches,
// --tags and --remotes reach no reference under refs/graft/, and git refuses --exclude-hidden
// before them. --alternate-refs walks the references of the object pool, which mirrors the
// member's references under git.ObjectPoolRefNamespace, refs/graft/ included: an agent's walk uses
// only the pool's branches and tags (core.alternateRefsPrefixes).

// excludeHiddenFlag makes git leave the references hidden from upload-pack out of the next --all
// or --glob.
const excludeHiddenFlag = "--exclude-hidden=uploadpack"

// agentWalkConfig is the configuration of an agent's command that walks git's pseudo references:
// the references --exclude-hidden=uploadpack leaves out, and the references of the object pool
// that --alternate-refs walks.
func agentWalkConfig() []GlobalOption {
	return []GlobalOption{
		ConfigPair{Key: "uploadpack.hideRefs", Value: agentrefs.HiddenPrefix},
		ConfigPair{Key: "core.alternateRefsPrefixes", Value: git.ObjectPoolRefNamespace + "/heads/ " + git.ObjectPoolRefNamespace + "/tags/"},
	}
}

// agentRefWalk reports whether sc is a command of an agent that walks git's pseudo references.
func agentRefWalk(ctx context.Context, sc Command) bool {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return false
	}
	description, ok := commandDescriptions[sc.Name]
	return ok && description.flags&scWalksRefs != 0
}

// walksAllRefs reports whether a pseudo reference argument reaches every reference, or every one
// a pattern matches: --all, --glob=<pattern>, or --glob before its pattern.
func walksAllRefs(arg string) bool {
	return arg == "--all" || arg == "--glob" || strings.HasPrefix(arg, "--glob=")
}

// excludeHiddenRefs returns sc with excludeHiddenFlag before each pseudo reference that reaches
// every reference, among its options and its arguments. An option is matched as a whole, so that
// the value of an option, which git does not take for a pseudo reference, is left alone.
func excludeHiddenRefs(sc Command) Command {
	flags := make([]Option, 0, len(sc.Flags)+1)
	for _, o := range sc.Flags {
		switch f := o.(type) {
		case Flag:
			if walksAllRefs(f.Name) {
				flags = append(flags, Flag{Name: excludeHiddenFlag})
			}
		case ValueFlag:
			if f.Name == "--glob" {
				flags = append(flags, Flag{Name: excludeHiddenFlag})
			}
		}
		flags = append(flags, o)
	}
	sc.Flags = flags

	args := make([]string, 0, len(sc.Args))
	for _, a := range sc.Args {
		if walksAllRefs(a) {
			args = append(args, excludeHiddenFlag)
		}
		args = append(args, a)
	}
	sc.Args = args
	return sc
}

// readsRevisionsFromStdin reports whether sc reads revisions, and so pseudo references, from its
// stdin.
func readsRevisionsFromStdin(sc Command) bool {
	for _, o := range sc.Flags {
		if f, ok := o.(Flag); ok && f.Name == "--stdin" {
			return true
		}
	}
	return false
}

// gitStdinLine returns the line of a revision list on git's stdin as git reads it: without its line
// feed and the carriage return before it (strbuf_getline), and cut at the first NUL, which ends a
// C string. A filter that compares anything else would be told "--all\r" is not "--all" when git
// takes it for "--all".
func gitStdinLine(line string) string {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	line, _, _ = strings.Cut(line, "\x00")
	return line
}

// excludeHiddenRefsOnStdin is the stdin of a command that reads revisions there, one per line,
// with excludeHiddenFlag on a line of its own before each --all and --glob=<pattern>, the lines
// parsed as git parses them (gitStdinLine). A line "--" ends the revisions: paths follow, which are
// left alone. It reads r as it is read, a line at a time, and runs nothing of its own.
func excludeHiddenRefsOnStdin(r io.Reader) io.Reader {
	return &excludeHiddenReader{in: bufio.NewReader(r)}
}

// excludeHiddenReader is the reader of excludeHiddenRefsOnStdin.
type excludeHiddenReader struct {
	in      *bufio.Reader
	pending []byte
	paths   bool
	err     error
}

func (r *excludeHiddenReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		var line string
		line, r.err = r.in.ReadString('\n')
		revision := gitStdinLine(line)
		if !r.paths && (revision == "--all" || strings.HasPrefix(revision, "--glob=")) {
			r.pending = append(r.pending, excludeHiddenFlag+"\n"...)
		}
		r.paths = r.paths || revision == "--"
		r.pending = append(r.pending, line...)
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
