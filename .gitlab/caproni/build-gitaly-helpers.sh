#!/usr/bin/env bash
# Rebuild the helper binaries for Linux when their source changes. They get
# embedded into the gitaly binary, so this runs before every ko build and
# from skaffold-prepare.sh.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "${REPO_ROOT}"

goarch=$(go env GOARCH)

aux=(gitaly-hooks gitaly-gpg gitaly-ssh gitaly-lfs-smudge)

matches_arch() {
  case "${goarch}" in
    amd64) grep -q 'x86-64' ;;
    arm64) grep -q 'aarch64' ;;
  esac
}

needs_rebuild() {
  local bin="$1" pkg="$2"
  if [ ! -x "${bin}" ] || file "${bin}" | grep -q Mach-O || ! file "${bin}" | matches_arch; then
    return 0
  fi
  [ -n "$(find "cmd/${pkg}" internal proto/go -name '*.go' -newer "${bin}" -print -quit)" ]
}

mkdir -p _build/bin

for b in "${aux[@]}"; do
  if needs_rebuild "_build/bin/${b}" "${b}"; then
    echo "building aux binary ${b} for linux/${goarch}"
    GOOS=linux GOARCH="${goarch}" go build -o "_build/bin/${b}" "./cmd/${b}"
  fi
done
