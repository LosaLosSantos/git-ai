package resolve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A git that says what the test wants: the regions of the strategy are the ones git's output gives, not the
// ones our own diff finds. The fake reads its labels and marker size from its arguments, as the real one
// does, and prints one conflict (exit 1), or none (exit 0) when the file named MERGE_CLEAN is in the
// directory it runs in.
const fakeGit = `#!/bin/sh
size=7
ours=; base=; theirs=
while [ $# -gt 0 ]; do
  case "$1" in
    --marker-size=*) size=${1#--marker-size=} ;;
    -L) shift; if [ -z "$ours" ]; then ours=$1; elif [ -z "$base" ]; then base=$1; else theirs=$1; fi ;;
  esac
  shift
done
rep() { printf "%${size}s" "" | tr " " "$1"; }
if [ -f FAKE_CLEAN ]; then cat ours; exit 0; fi
printf 'head\n'
echo "$(rep '<') $ours"
printf 'from git ours\n'
echo "$(rep '|') $base"
printf 'from git base\n'
rep '='; echo
printf 'from git theirs\n'
echo "$(rep '>') $theirs"
printf 'tail\n'
exit 1
`

func TestLLMFileRegionsAreTheOnesGitGives(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The three versions are identical: our own diff finds nothing to ask. git (the fake) says there is a
	// conflict, and that is the region the model is asked about.
	same := []byte("one\ntwo\n")
	regions, err := llmFileRegions(context.Background(), same, same, same, true)
	if err != nil {
		t.Fatal(err)
	}
	var conflicts []llmRegion
	for _, r := range regions {
		if !r.clean {
			conflicts = append(conflicts, r)
		}
	}
	if len(conflicts) != 1 || conflicts[0].ours != "from git ours\n" || conflicts[0].base != "from git base\n" || conflicts[0].theirs != "from git theirs\n" {
		t.Fatalf("conflicts %+v", conflicts)
	}
	if got := llmJoin(regionsWithAnswers(regions, "answer\n")); got != "head\nanswer\ntail\n" {
		t.Fatalf("joined %q", got)
	}
	if n := LLMConflictRegionsBy(same, same, same, true); n != 1 {
		t.Fatalf("regions by git: %d", n)
	}
	if n := LLMConflictRegionsBy(same, same, same, false); n != 0 {
		t.Fatalf("regions by our diff: %d", n)
	}

	// With the option off the regions are those of our own diff, whatever git says.
	regions, err = llmFileRegions(context.Background(), same, same, same, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range regions {
		if !r.clean {
			t.Fatalf("a conflict by our diff: %+v", r)
		}
	}

	// A git that fails, or whose output is not what a merge writes, is not believed: our diff answers.
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\necho nonsense\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	regions, err = llmFileRegions(context.Background(), same, same, same, true)
	if err != nil || strings.Contains(llmJoin(regions), "nonsense") {
		t.Fatalf("regions %+v, err %v", regions, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 200\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if regions, err = llmFileRegions(context.Background(), same, same, same, true); err != nil || llmJoin(regions) != "one\ntwo\n" {
		t.Fatalf("regions %+v, err %v", regions, err)
	}
}
