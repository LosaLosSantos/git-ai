#!/bin/sh
# benchmark-push TRANSPORT NAME RELATIVE_PATH SIZE_BYTES FILE_NAME MESSAGE BRANCH [REF_COUNT] [GL_REPOSITORY]
#
# Real `git push` of one new commit to repository NAME, as REF_COUNT new
# branches. The commit holds one FILE_NAME blob of SIZE_BYTES random data
# on top of the local clone's HEAD, so the server sees an ordinary branch
# push onto existing history: advertisement, pack-objects, sideband.
#
# Each push is a stateless process sharing one bare clone per repository:
# the commit is built with plumbing, and new objects land in a throwaway
# directory layered over the clone via alternates, so the clone stays
# byte-identical and client-side cost is constant for the whole run.
#
# TRANSPORT: "ssh" goes through gitlab-shell over real ssh to
# SSHReceivePack; "http" through workhorse to PostReceivePack.
#
# Prints "PUSH_MS <millis>" (wall time of `git push` alone) and
# "PUSH_OK <oid>" on success; k6 parses both. xk6-exec merges stderr into
# the output, so the success path stays quiet.
set -eu

transport=$1
name=$2
size=$4
fname=$5
msg=$6
branch=$7
ref_count=${8:-1}

clone="${CLONES_DIR:-/opt/benchmark-gitaly/clones}/${name}.git"
if [ ! -d "$clone" ]; then
    echo "clone ${clone} missing (entrypoint clone failed?)" >&2
    exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Throwaway object dir over the shared clone via alternates; concurrent
# VUs never write into the clone, per-push objects vanish with $tmp.
export GIT_DIR="$clone"
export GIT_OBJECT_DIRECTORY="$tmp/objects"
export GIT_ALTERNATE_OBJECT_DIRECTORIES="$clone/objects"
mkdir -p "$GIT_OBJECT_DIRECTORY"

parent=$(git rev-parse HEAD)
head -c "$size" /dev/urandom > "$tmp/blob"
blob=$(git hash-object -w -t blob "$tmp/blob")
tree=$(printf '100644 blob %s\t%s\n' "$blob" "$fname" | git mktree)

export GIT_AUTHOR_NAME='Benchmark User' GIT_AUTHOR_EMAIL='benchmark@example.com'
export GIT_COMMITTER_NAME='Benchmark User' GIT_COMMITTER_EMAIL='benchmark@example.com'
commit=$(git commit-tree "$tree" -p "$parent" -m "$msg")

refspecs=""
if [ "$ref_count" -gt 1 ]; then
    i=0
    while [ "$i" -lt "$ref_count" ]; do
        refspecs="$refspecs $commit:refs/heads/$branch-$i"
        i=$((i + 1))
    done
else
    refspecs="$commit:refs/heads/$branch"
fi

# Bound the push: xk6-exec has no timeout and k6 cannot interrupt a VU
# stuck in exec, so one push hung on a dead connection wedges the whole
# run after its last section. Any healthy push here finishes in seconds;
# on expiry the non-zero exit surfaces as PUSH_FAIL in k6.
push_timeout=${PUSH_TIMEOUT_SECONDS:-120}

# GNU date; the image is Debian. %3N = milliseconds.
start_ms=$(date +%s%3N)
case "$transport" in
ssh)
    # gitlab-shell authenticates the key and derives the repository and
    # user itself, so there is no GITALY_PAYLOAD to assert.
    # shellcheck disable=SC2086 # refspecs is a deliberate word list
    GIT_SSH_COMMAND="${BENCH_SSH_COMMAND:?ssh transport needs BENCH_SSH_COMMAND}" \
        timeout "$push_timeout" git push --quiet \
        "${GITLAB_SSH_URL:?ssh transport needs GITLAB_SSH_URL}/${BENCH_GROUP:-bench}/${name}.git" \
        $refspecs
    ;;
http)
    if [ -z "${GITLAB_PAT:-}" ]; then
        echo 'GITLAB_PAT unset; re-run provision-repos to mint it' >&2
        exit 1
    fi
    # Splice the PAT into GITLAB_HTTP_URL and keep its scheme.
    scheme="${GITLAB_HTTP_URL%%://*}"
    host="${GITLAB_HTTP_URL#*://}"
    # shellcheck disable=SC2086
    timeout "$push_timeout" git push --quiet \
        "${scheme}://oauth2:${GITLAB_PAT}@${host}/${BENCH_GROUP:-bench}/${name}.git" \
        $refspecs
    ;;
*)
    echo "unknown transport: ${transport}" >&2
    exit 1
    ;;
esac
end_ms=$(date +%s%3N)

echo "PUSH_MS $((end_ms - start_ms))"
echo "PUSH_OK $commit"
