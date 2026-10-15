# Fuzzing

Graft uses Go's native fuzzing (`testing.F`) on the parsers and decoders that
read data from outside the process: git output, hook input, the changeset log,
the resolver protocol, the standalone policy, and the blame output.

Every fuzz target has a seed corpus made of realistic inputs (copied from the
unit tests or from the wire formats). The seeds run in every `go test` run, so a
regression on a seed fails the normal test suite without needing `-fuzz`.

## Targets

| Package | File | Target | Properties checked |
| --- | --- | --- | --- |
| `internal/git/localrepo` | `merge_fuzz_test.go` | `FuzzParseMergeTreeError` | no panic, deterministic result, every accepted conflict is reproduced field by field: one conflicting file per line, every message's paths, type and text come from the input, no field is dropped or invented |
| `internal/graft/changelog` | `log_fuzz_test.go` | `FuzzParseHookLine` | accepted line reassembles exactly, no ASCII space or control in object IDs |
| | | `FuzzParseUpdates` | only branches and tags, no no-op updates, every update is a valid hook line |
| | | `FuzzEntryJSON` | a decoded entry encodes and decodes to the same entry |
| | | `FuzzEntryRoundTrip` | an entry built from arbitrary fields keeps its reference byte for byte |
| `internal/graft/resolver` | `resolver_fuzz_test.go` | `FuzzValidate` | an accepted answer answers each conflicted path exactly once, with valid modes and within the limit |
| | | `FuzzSideJSON` | the base64 side encoding round-trips |
| | | `FuzzAllowed` | unclean paths are never allowed, deny wins, literal and trailing-`**` globs match exactly |
| `internal/gitlab` | `policy_fuzz_test.go` | `FuzzParseChangedRefs` | every returned reference forms a valid hook line |
| | | `FuzzMatchRef` | literal, `/**` and trailing-`**` globs match exactly their literal part |
| | | `FuzzEvaluatePolicy` | an allowed change set only contains references the rule allows |
| `internal/graft/derived` | `blame_fuzz_test.go` | `FuzzParsePorcelain` | deterministic, sorted by line, valid commit IDs, no newline in content |
| `internal/git/gitcmd` | `hooks_payload_fuzz_test.go` | `FuzzChangelogMetaFromEnv` | no panic on arbitrary environment values, deterministic (whether it fails, and the whole decoded metadata), encoded metadata round-trips |
| `internal/graft/review/classify` | `fuzz_test.go` | `FuzzGoMatchesGoScanner` | whenever a Go change is classified COMMENT_ONLY, `go/scanner` sees the same token stream (comments dropped, automatic semicolons kept) in both versions |
| | | `FuzzFile` | no panic for arbitrary paths and contents (also added and deleted), deterministic, always a reason |
| | | `FuzzTokenize` | every lexer terminates, deterministic, no empty token |
| `tools/graft-agents/protocol` | `protocol_fuzz_test.go` | `FuzzRequestDecode` | a decoded request re-encodes to the same files, sides and contents |
| | | `FuzzResponseRoundTrip` | content survives the round trip, deletes carry null content |

## Running

Run the seeds only (what CI does):

    go test ./internal/graft/changelog ./internal/graft/resolver ./internal/gitlab ./internal/graft/derived ./internal/git/gitcmd ./internal/git/localrepo ./internal/graft/review/classify

Run one fuzzer for 20 seconds (one fuzzer per command, since `-fuzz` accepts a
single target):

    go test -run=^$ -fuzz=FuzzParseHookLine -fuzztime=20s ./internal/graft/changelog

For `tools/graft-agents`, use the module directory:

    go -C tools/graft-agents test -run=^$ -fuzz=FuzzRequestDecode -fuzztime=20s ./protocol

Go stores any failing input under `testdata/fuzz/<FuzzName>/` in the package
directory. Such a file is a regression case: the seed run of the test reproduces
it, and it stays failing until the bug is fixed.

## Latest 20-second runs

Each target ran alone for 20 seconds (`-fuzztime=20s`) on 2026-10-15, on a
shared development machine (other test suites were running, so the counts are
indicative only). The fixes of the findings below were in place. No target
found a failing input.

| Target | Inputs executed in 20 s |
| --- | --- |
| `FuzzParseMergeTreeError` | 153,216 |
| `FuzzParseHookLine` | 1,672,023 |
| `FuzzParseUpdates` | 1,864,655 |
| `FuzzEntryJSON` | 2,102,984 |
| `FuzzEntryRoundTrip` | 266,595 |
| `FuzzValidate` | 1,055,044 |
| `FuzzSideJSON` | 926,433 |
| `FuzzAllowed` | 897,435 |
| `FuzzParseChangedRefs` | 771,993 |
| `FuzzMatchRef` | 625,794 |
| `FuzzEvaluatePolicy` | 591,245 |
| `FuzzParsePorcelain` | 119,266 |
| `FuzzChangelogMetaFromEnv` | 437,510 |
| `FuzzGoMatchesGoScanner` | 1,128,689 |
| `FuzzFile` | 331,975 |
| `FuzzTokenize` | 185,787 |
| `FuzzRequestDecode` | 7,911,082 |
| `FuzzResponseRoundTrip` | 1,156,749 |

## Findings

### Merge-tree output: path count panics

`parseMergeTreeError` (`internal/git/localrepo/merge.go`) reads a path count
from the conflict info section with `strconv.Atoi`, which accepts any integer.
Two inputs panicked:

- A negative count passes the bounds check and the slice
  `fields[i+1 : i+numOfPaths+1]` then has a reversed or negative bound:

      slice bounds out of range [1:0]

  A count of -3 never advances the loop, so it also loops forever.
- A huge count such as 9223372036854775807 makes `i+numOfPaths+2` wrap to a
  negative value, which passes the `>= len(fields)` check and panics in the
  slice:

      slice bounds out of range [:-9223372036854775808]

The fix, in `merge.go`, rejects a count that is negative or larger than the
fields that remain after it, so that no sum can overflow:

    if numOfPaths < 0 || numOfPaths > len(fields)-i-3 {
        return "", structerr.NewInternal("incorrect number of fields: %s", infoMsg)
    }

Both inputs are fixed regression seeds. Their corpus entries are stored in
`internal/git/localrepo/testdata/fuzz/FuzzParseMergeTreeError/`:
`negative-path-count`, `huge-path-count`, `path-count-overflow` and
`path-count-near-overflow` (the largest counts that wrap). The seed run of
`FuzzParseMergeTreeError` runs them in every `go test` run, so a regression
fails the normal test suite.

Both panics were found by `FuzzParseMergeTreeError`, from inputs with a negative
and a huge path count (the comment above the check in `merge.go` says so too).
Random mutation alone rarely produces such extreme integers in a 20-second run,
so the inputs are kept as explicit seeds rather than left to the mutator.

### Review classifier: Go spellings that differ but lexed alike

`FuzzGoMatchesGoScanner` found two pairs of Go sources that the classifier
called COMMENT_ONLY although `go/scanner` reads different tokens. Either would
let a change pass review with the approvals of a comment fix.

- `>>> >` and `>>>>`. The Go, Java and Rust lexers shared one operator list,
  a superset of the three languages, so the Go lexer munched Java's `>>>`:
  both sources lexed as `>>>` `>`, while Go reads `>>` `>` `>` and `>>` `>>`.
- `.666_0p0` and `. 666_0p0`. A dot followed by a digit starts a floating-point
  literal in Go, so the first is one literal and the second a dot and a
  number; the lexer read a dot and a number in both.

The fix, in `internal/graft/review/classify/lex_c.go`, gives each language
its exact operator set (`goOps`, `javaOps`, `rustOps`), so runs of operator
characters split the way the language's own scanner splits them, and lexes a
dot followed by a digit as the start of a number in Go and Java (in Rust it is
a tuple field access, `t.0`). Both inputs are regression seeds in
`internal/graft/review/classify/testdata/fuzz/FuzzGoMatchesGoScanner/`, and
`TestOperatorsAreLanguageExact` covers them and the same pattern in Java and
Rust. After the fix the target ran 90 seconds (4.4 million inputs) without a
finding.

### Changelog entry round trip with invalid UTF-8 object IDs

`Entry.OldOID` and `Entry.NewOID` are plain JSON strings. Object IDs are hex by
contract, so this is not a bug in practice, but a non-UTF-8 value would not
round-trip. The fuzz target therefore compares object IDs and operations only when
they are valid UTF-8. Only `Reference` carries the `reference_b64` escape hatch.

## Adding a target

1. Add a `FuzzX(f *testing.F)` in a `*_fuzz_test.go` file of the package.
2. Seed it with realistic inputs from the package's unit tests.
3. Assert properties (no panic, round trip, invariants), not exact outputs.
4. Run it for 20 seconds before merging, and commit any failing input under
   `testdata/fuzz` only together with the fix or with a documented finding here.
