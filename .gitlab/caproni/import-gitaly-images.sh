#!/usr/bin/env bash
# Import local gitaly images into cluster containerd so the pod can
# pull the tag skaffold deployed.

set -euo pipefail

k3d_node=$(docker ps --format '{{.Names}}' 2>/dev/null | grep '^k3d-.*-server-0$' | head -1 || true)
if [ -n "${k3d_node}" ]; then
  ctr_ls() { docker exec "${k3d_node}" ctr -n k8s.io images ls -q; }
  ctr_import() { docker exec -i "${k3d_node}" ctr -n k8s.io images import -; }
elif colima status --profile caproni >/dev/null 2>&1; then
  ctr_ls() { colima ssh --profile caproni -- sudo ctr -n k8s.io images ls -q; }
  ctr_import() { colima ssh --profile caproni -- sudo ctr -n k8s.io images import -; }
else
  echo "error: no k3d node or colima caproni profile found. Is the cluster up (\`caproni up\`)?" >&2
  exit 1
fi

if ! existing=$(ctr_ls 2>&1); then
  echo "error: could not reach the cluster containerd. Is the cluster up (\`caproni up\`)?" >&2
  exit 1
fi

num=0
for img in $(docker images --format '{{.ID}}' registry.gitlab.com/gitlab-org/build/cng/gitaly | sort -u); do
  if ! tags=$(docker image inspect --format '{{range .RepoTags}}{{println .}}{{end}}' "$img" 2>/dev/null); then
    continue # skip this image since it may have already been retagged or removed
  fi

  skip=false
  while IFS= read -r tag; do
    [ -z "${tag}" ] && continue
    tag_name=${tag##*:}
    # Skip version tags since the same name can be reused for a new build.
    [[ "${tag_name}" != *[!0-9a-f]* && ${#tag_name} -eq 64 ]] || continue
    if grep -Fqx "${tag}" <<< "${existing}"; then
      echo "already imported: ${tag}"
      skip=true
      break
    fi
  done <<< "${tags}"

  if [ "${skip}" = true ]; then
    continue
  fi

  # Save by tag since that's what the pod actually pulls by.
  tag_array=()
  while IFS= read -r t; do
    [ -z "${t}" ] && continue
    tag_array+=("${t}")
  done <<< "${tags}"
  if docker save "${tag_array[@]}" | ctr_import; then
    num=$((num + 1))
  fi
done

if [ "${num}" -eq 0 ]; then
  echo "no local gitaly images to import"
fi
