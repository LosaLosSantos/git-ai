# Code search

Graft adds a server-side code search so that agents can find code without
cloning a repository. `SearchService.SearchCode` runs `git grep` on the server
at one commit and streams the matching lines back. The SDK exposes it as
`Repo.SearchCode`, and the CLI as `graft search`.

Code: `internal/gitaly/service/search` (server), `sdk/search.go` (SDK),
`cmd/graft/search.go` (CLI). Protocol: `proto/search.proto`.

## RPC: SearchService.SearchCode

`SearchCode` is an accessor with a server-streaming response.

| Request field | Meaning |
| --- | --- |
| `repository` | Repository to search. |
| `revision` | The commit to search at: a branch, a tag or a commit ID. It is resolved with `^{commit}`, so a tag is peeled. A range (`a..b`) is refused. |
| `query` | The text to search for. It must not be empty, must be at most 1 KiB, and must not contain a NUL byte or a line break. Matching is per line. |
| `regex` | Read `query` as a POSIX extended regular expression (`git grep --extended-regexp`). Otherwise it is a fixed string (`--fixed-strings`). |
| `case_sensitive` | Match case. **The default is case-insensitive** (`--ignore-case`). |
| `paths` | Limit the search to these paths. Each one is passed to git as a pathspec after `--`: a file or a directory prefix, and git glob characters work. A path must be relative and clean (no `..`, no `//`, no trailing `/`), and must not start with `:` (git pathspec magic). Empty means the whole tree. |
| `max_results` | The largest number of matching lines to return. 0 means the server maximum. A larger value is capped at the server maximum. |
| `context_lines` | Lines of context before and after each match, capped at the server maximum. |

Each `SearchCodeResponse` carries a batch of `Match` messages: `path`,
`line_number` (1-based), `line` (the matching line without its newline),
`preceding_lines` and `following_lines`. `commit_id`, the resolved commit, is
set on the first message only. `truncated` is set on the last message when the
results were cut short.

Behavior:

- The server runs `git grep --null --line-number --full-name --no-color -I
  [--fixed-strings|--extended-regexp] [--ignore-case] -e <query> <commit> --
  <paths...>` through the normal git command factory, with no shell.
- Matches come in the order git lists them: by path in tree order, then by line.
- A batch holds at most 100 matches and about 1 MiB of encoded matches, so no
  message comes close to the 4 MiB gRPC receive limit. A single match that is
  larger than 1 MiB on its own (only a very long path can cause that) is sent
  in a message by itself.
- Lines longer than 4 KiB, match lines and context lines alike, are cut to
  4 KiB. When the line is valid UTF-8 the cut falls on a character boundary.
  The server drops the rest of an overlong git grep output line as it reads
  it, so a huge line (a minified file) is never held in memory whole.
- Binary files are skipped (`-I`).
- `git grep` exit status 1 (no match) is not an error: the stream holds one
  message with the commit ID and no matches.
- When `max_results` is reached and more matches exist, the server stops git
  grep and sets `truncated`. When the time budget runs out, the matches found
  so far are returned with `truncated` set; this is not an error.
- Context lines are not read from git grep. With `--null`, git separates match
  lines and context lines the same way, so they cannot be told apart. The
  server reads the blob of each file that has a match instead (one file at a
  time, through the object reader) and takes the lines around the match from
  it.

Errors:

| Code | When |
| --- | --- |
| `InvalidArgument` | Invalid repository, empty or oversized query, NUL or line break in the query, invalid revision or a range, an invalid path, or an invalid regular expression (git grep exit status 128 with `regex` set). |
| `NotFound` | The revision does not name a commit. |
| `Canceled` | The client canceled the call. |
| `Internal` | git grep failed for another reason (stderr is attached as metadata), or its output could not be parsed. |

Agents: every SearchService RPC is in the agent allowlist
(`internal/gitaly/server/auth/agent_methods.go`); it only reads a commit.

## Configuration: `[graft.search]`

```toml
[graft.search]
max_results = 1000        # cap of matches per call (default 1000)
max_context_lines = 5     # cap of context lines per side (default 5)
timeout = "10s"           # time budget of one call, a Go duration (default 10s)
```

Zero or an empty value means the default; so does a negative `max_results` or
`max_context_lines`. A `timeout` that does not parse or is not positive is
rejected when the configuration is loaded.

## SDK

```go
type SearchOptions struct {
    Revision, Query      string
    Regex, CaseSensitive bool
    Paths                []string
    MaxResults, ContextLines int
}

type SearchMatch struct {
    Path   string
    Line   int
    Text   string
    Before, After []string
}

func (r *Repo) SearchCode(ctx context.Context, opts SearchOptions) (matches []SearchMatch, commitID string, truncated bool, err error)
```

`SearchCode` collects the whole stream. `Revision` and `Query` are required;
the SDK checks the query length and the ranges of `MaxResults` and
`ContextLines` before it calls the server. As in the RPC, `CaseSensitive` is
false by default, so matching is case-insensitive unless it is set. `Path` and
the lines hold the exact bytes of the repository, which need not be valid
UTF-8.

## CLI: `graft search`

```text
graft search QUERY [--regex] [-i | --ignore-case] [--path P]... [--context N] [-n MAX] [--rev REV] [--json]
```

`graft search` runs inside a workspace and searches the workspace's repository
on its server, at the workspace branch or at `--rev`.

- **The CLI is case-sensitive by default**, like grep. `-i` or `--ignore-case`
  turns case-insensitive matching on. (The RPC and the SDK default to
  case-insensitive; the CLI sets `case_sensitive` itself.)
- `--path` is repeatable. Each path is checked like a workspace path before the
  call.
- `--context N` prints N lines of context. `-C` is not available for this: it
  is the workspace directory flag of every graft command.
- `-n MAX` caps the matches (default: the server limit).
- Output is grep-like: `path:line:text` for a match, `path-line-text` for a
  context line, and `--` between groups that are not adjacent. Context lines
  already printed are not repeated, and a match is never printed as context.
  Paths and lines are printed through the same escaping as other graft output,
  so control characters in a file cannot forge output lines.
- `--json` prints one JSON object per match: `path`, `line`, `text`, `before`,
  `after`. When the path or a line is not valid UTF-8, the readable field has
  U+FFFD in place of the invalid bytes, and `path_b64`, `text_b64`,
  `before_b64` or `after_b64` carries the exact bytes in base64 (for `before`
  and `after`, every line of the list is included so the indexes match). Those
  fields are absent when the readable form is exact.
- When the results were truncated, a note goes to standard error.
- A search without matches prints nothing and exits 0, unlike grep.

## Limitations

- There is no index. Every call runs `git grep` over the tree of the commit, so
  the cost grows with the size of the repository; the time budget (`timeout`)
  bounds it, and a search that runs out of time returns partial results with
  `truncated` set. The proto allows the server to cache results per commit;
  the server does not do that yet.
- For context lines, the server reads the whole blob of each file with a match
  into memory. A very large text file with a match costs its full size in
  memory for the duration of that file. It also lists the blobs of the searched
  paths (the whole tree when a path has a glob character) once per call.
  Memory per call has no bound other than these sizes and the time budget.
- A line longer than 4 KiB is cut, so the part of the line that matched may not
  be in the returned text.
- Submodules are not searched, and Git LFS pointer files are searched as the
  small text files they are, not as the content they point to.
- The query is one line: git grep would treat a multi-line `-e` pattern as
  several alternative patterns, so a line break is refused instead.
- Only the selected commit is searched; there is no search across history or
  across branches.
