.PHONY: build-benchmark-image push-benchmark-image provision-repos

# Repo root, used to stage gitaly proto files into the docker build context.
REPO_ROOT      ?= $(realpath $(CURDIR)/../..)
IMAGE_CONTEXT  := $(CURDIR)/docker/benchmark-client
IMAGE_NAME     ?= benchmark-client
# Tag = <git-short-sha>-<unix-timestamp>: SHA for traceability, timestamp for
# per-build uniqueness so concurrent builds never overwrite each other.
IMAGE_TAG      ?= $(shell git -C $(REPO_ROOT) rev-parse --short HEAD 2>/dev/null || echo dev)-$(shell date +%s)
# Default registry is the Artifact Registry repo for the gitaly-benchmark
# GCP project. Override IMAGE_REGISTRY to push elsewhere. The location prefix
# (us-east1-docker.pkg.dev) requires a one-time `gcloud auth configure-docker
# us-east1-docker.pkg.dev` on the build host.
IMAGE_REGISTRY ?= us-east1-docker.pkg.dev/gitaly-benchmark-0150d6cf/gitaly-benchmarking
IMAGE_REF      := $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
# Floating "latest" tag pushed alongside the SHA tag so a bare image
# reference (no :tag) resolves to the most recent build. Defends against
# the case where BENCHMARK_IMAGE is exported tag-less in the operator's
# shell — Kubernetes would otherwise default to :latest and ErrImagePull.
IMAGE_REF_LATEST := $(IMAGE_REGISTRY)/$(IMAGE_NAME):latest
# Records the exact ref of the last push; run-benchmarks-incluster reads it
# so a run uses that image instead of :latest. Gitignored, per-checkout.
LAST_IMAGE_FILE := $(CURDIR)/.last-pushed-image
# Target platform for the image. GKE nodes are linux/amd64; building on an
# Apple Silicon Mac defaults to linux/arm64 which the cluster cannot run
# ("exec format error" inside the container). Override only if you also
# run the cluster on arm64 nodes (e.g. T2A node pool).
IMAGE_PLATFORM ?= linux/amd64

# TLS-intercepting endpoint agents (e.g. Koi) re-sign container HTTPS
# traffic with a private CA, breaking go mod/pip/curl inside `docker
# build`. Stage the host's CAs into the context; empty when none.
define stage_host_ca_bundle
	{ security find-certificate -a -p /Library/Keychains/System.keychain 2>/dev/null \
		|| cat /etc/ssl/certs/ca-certificates.crt 2>/dev/null \
		|| true; } > $(IMAGE_CONTEXT)/host-ca-bundle.crt
endef

## Build and push the benchmark-client image for $(IMAGE_PLATFORM).
## Uses `docker buildx` so cross-building from an arm64 Mac to amd64
## GKE nodes works out of the box. Stages gitaly proto/ into the build
## context so the image bakes a self-contained proto tree.
##
## Note: buildx with --push builds and pushes in one step; there is no
## standalone build target because the loaded local image cannot carry a
## platform that differs from the host's architecture (Docker would
## silently substitute the native arch and the next push would re-upload
## the wrong manifest).
push-benchmark-image:
	@rm -rf $(IMAGE_CONTEXT)/proto $(IMAGE_CONTEXT)/host-ca-bundle.crt
	cp -R $(REPO_ROOT)/proto $(IMAGE_CONTEXT)/proto
	$(stage_host_ca_bundle)
	docker buildx build \
		--platform $(IMAGE_PLATFORM) \
		-t $(IMAGE_REF) \
		-t $(IMAGE_REF_LATEST) \
		--push \
		$(IMAGE_CONTEXT)
	@rm -rf $(IMAGE_CONTEXT)/proto $(IMAGE_CONTEXT)/host-ca-bundle.crt
	@printf '%s\n' '$(IMAGE_REF)' > $(LAST_IMAGE_FILE)
	@echo "pushed $(IMAGE_REF) ($(IMAGE_PLATFORM)) + :latest; recorded in $(LAST_IMAGE_FILE) (run-benchmarks-incluster uses it by default)"

## Provision benchmark repositories in the chart-installed GitLab and
## record their hashed disk paths in experiments/$EXPERIMENT/config.yml.
## One-shot per cluster; safe to re-run (idempotent).
##
## Sources env/bin/activate if present so PyYAML / ruamel.yaml are
## importable. Falls back to system python3 (in which case the operator
## needs `pip install -r requirements.txt` against their host python).
##
## Required: EXPERIMENT=<name>
## Optional: GITLAB_NAMESPACE (default: gitlab), BENCH_GROUP (default: bench)
provision-repos:
	@[ -n "$$EXPERIMENT" ] || { echo "error: EXPERIMENT must be set" >&2; exit 1; }
	@if [ -f env/bin/activate ]; then \
		. env/bin/activate && ./incluster/provision-repos --write-config; \
	else \
		./incluster/provision-repos --write-config; \
	fi

## Build only, for native architecture, into the local Docker daemon.
## Useful for inspecting the image with `docker run` on the build host;
## not for pushing to the cluster.
build-benchmark-image:
	@rm -rf $(IMAGE_CONTEXT)/proto $(IMAGE_CONTEXT)/host-ca-bundle.crt
	cp -R $(REPO_ROOT)/proto $(IMAGE_CONTEXT)/proto
	$(stage_host_ca_bundle)
	docker build -t $(IMAGE_REF) -t $(IMAGE_REF_LATEST) $(IMAGE_CONTEXT)
	@rm -rf $(IMAGE_CONTEXT)/proto $(IMAGE_CONTEXT)/host-ca-bundle.crt
	@echo "built $(IMAGE_REF) for native arch (use push-benchmark-image to publish)"
