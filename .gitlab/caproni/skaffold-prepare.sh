#!/usr/bin/env bash
# Prepare hook (edit_mode_start): build gitaly and import into cluster containerd.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

# Gitaly stores Git data on disk, so it runs in a StatefulSet. We generate it
# from the Helm chart here instead of committing one that drifts.
render_edit_mode_statefulset() {
  local out err
  out=$(mktemp)
  err=$(mktemp)
  trap 'rm -f "${out}" "${err}"' RETURN

  if ! caproni tool helm get manifest gitlab --namespace gitlab 2>"${err}" | yq -oy eval '
    select(.kind == "StatefulSet" and .metadata.name == "gitlab-gitaly") |
    .spec.template.spec.containers[0].command = ["/ko-app/gitaly", "serve", "/etc/gitaly/config.toml"] |
    .spec.template.spec.containers[0].volumeMounts |= with(.[]; select(.name == "gitaly-config") | .mountPath = "/etc/gitaly")
  ' - > "${out}" || [ ! -s "${out}" ]; then
    echo "error: no gitlab-gitaly StatefulSet found. Is the cluster up (\`caproni up\`)?" >&2
    if [ -s "${err}" ]; then
      cat "${err}" >&2
    fi
    rm -f "${out}" "${err}"
    exit 1
  fi

  chmod 644 "${out}"
  mv "${out}" "${REPO_ROOT}/k8s/statefulset.yaml"
  echo "wrote ${REPO_ROOT}/k8s/statefulset.yaml"
}

render_edit_mode_statefulset
unset -f render_edit_mode_statefulset

case "$(uname -m)" in
  x86_64|amd64)
    goarch=amd64
    ;;
  *)
    goarch=arm64
    ;;
esac

"$(dirname "$0")/build-gitaly-helpers.sh"

match_arch() {
  case "${goarch}" in
    amd64) grep -q 'x86-64' ;;
    arm64) grep -q 'aarch64' ;;
  esac
}

# Git binaries are embedded into the gitaly image, so they must be Linux.
git_execs=(git git-http-backend git-remote-http)
git_versions=(mvcc master prev)

need_git_build=0
for e in "${git_execs[@]}"; do
  for v in "${git_versions[@]}"; do
    f="_build/bin/gitaly-${e}-${v}"
    if [ ! -x "${f}" ] || file "${f}" | grep -q Mach-O || ! file "${f}" | match_arch; then
      need_git_build=1
      break 2
    fi
  done
done

if [ "${need_git_build}" = 1 ]; then
  echo "building linux/${goarch} bundled git binaries"
  docker build -q -t ko.local/gitaly-bundled-git-builder - < Dockerfile.bundled-git >/dev/null
  docker run --rm -v "$(pwd):/src" -w /src ko.local/gitaly-bundled-git-builder make build-bundled-git
fi

if docker image inspect ko.local/gitaly-base:latest &>/dev/null \
  || ! docker images --format '{{.Repository}}:{{.Tag}}' | grep -q '^ko.local/gitaly-base:'; then
  :
else
  tag=$(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^ko.local/gitaly-base:' | head -1)
  docker tag "${tag}" ko.local/gitaly-base:latest
fi

build_json="/tmp/gitaly-skaffold-build.json"
skaffold build --file-output="${build_json}" --platform "linux/${goarch}"

"$(dirname "$0")/import-gitaly-images.sh"
