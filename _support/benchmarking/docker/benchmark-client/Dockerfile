ARG GO_VERSION=1.25
ARG KUBECTL_VERSION=v1.30.4
ARG AGRIND_VERSION=v0.19.6
# xk6 v1.4.3+ requires Go >= 1.25, which is why GO_VERSION is bumped above.
# Pin xk6 to a known-good version rather than `latest` so future upstream
# Go-version bumps don't silently break the image build.
ARG XK6_VERSION=v1.4.3
ARG XK6_EXEC_VERSION=v0.5.1

# Pinned to BUILDPLATFORM (the host's native arch) rather than the target:
# running the Go toolchain under QEMU emulation to produce a linux/amd64
# binary on an arm64 Mac segfaults in `compile`/`asm`/`cc1`. Go cross-compiles
# natively instead, which is both reliable and much faster. TARGETOS/TARGETARCH
# are supplied by BuildKit from --platform.
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION}-trixie AS k6-builder
ARG XK6_VERSION
ARG XK6_EXEC_VERSION
ARG TARGETOS
ARG TARGETARCH
ENV GOPATH=/go
ENV PATH=/go/bin:/usr/local/go/bin:${PATH}
# Host CAs staged by the Makefile (possibly empty); without them a
# TLS-intercepting endpoint agent breaks the Go module downloads below.
COPY host-ca-bundle.crt /usr/local/share/ca-certificates/host-ca-bundle.crt
RUN update-ca-certificates
# xk6 itself is a build tool, so it runs on the build platform.
RUN go install go.k6.io/xk6/cmd/xk6@${XK6_VERSION}
WORKDIR /build
# CGO_ENABLED=0 keeps the cross-build free of a target-arch C toolchain; k6
# and xk6-exec are pure Go, and a static binary suits the scratch-ish runtime.
# github.com/grafana/xk6-exec stays the canonical module path even though
# the repo was archived: v0.5.1's go.mod declares it, and the module
# proxy serves it regardless.
RUN mkdir -p /out \
    && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
       xk6 build --with github.com/grafana/xk6-exec@${XK6_EXEC_VERSION} --output /out/k6

FROM debian:trixie-slim AS runtime
ARG KUBECTL_VERSION
ARG AGRIND_VERSION
# python3 + pandas + plotnine are used by plot.py to turn the harvested
# gitaly-<N>.log files into latency / throughput PNGs after k6 exits.
# pandas + numpy come from Debian so the heavy native deps stay on
# glibc-compatible wheels with no build toolchain. plotnine is PyPI-only
# (no Debian package exists), so we pip-install just that on top.
# --break-system-packages bypasses PEP 668: this is a single-purpose
# container image, not a shared system, so the safety net is irrelevant.
# Same host-CA staging as the k6-builder stage, for pip and curl.
COPY host-ca-bundle.crt /usr/local/share/ca-certificates/host-ca-bundle.crt
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git jq openssh-client tar \
        python3 python3-pip python3-pandas \
    && update-ca-certificates \
    && pip3 install --no-cache-dir --break-system-packages plotnine \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" -o /usr/local/bin/kubectl \
    && chmod 0755 /usr/local/bin/kubectl

# angle-grinder (agrind) computes per-RPC latency percentiles from the
# harvested Gitaly JSON logs (see entrypoint.sh). Static musl build, no
# extra runtime deps.
RUN curl -fsSL "https://github.com/rcoh/angle-grinder/releases/download/${AGRIND_VERSION}/agrind-x86_64-unknown-linux-musl.tar.gz" \
    | tar -xz -C /usr/local/bin agrind \
    && chmod 0755 /usr/local/bin/agrind

COPY --from=k6-builder /out/k6 /usr/local/bin/k6

# Gitaly proto files are baked at image-build time. The Makefile target
# copies the proto/ tree from the gitaly checkout into the build context
# before invoking docker build.
COPY proto /src/gitaly/proto

WORKDIR /opt/benchmark-gitaly

COPY entrypoint.sh /usr/local/bin/benchmark-entrypoint
# push-branch.sh drives a real `git push` per iteration (hence git above),
# reaching Gitaly through gitlab-shell over ssh (hence openssh-client) or
# through workhorse.
COPY push-branch.sh /usr/local/bin/benchmark-push
RUN chmod 0755 /usr/local/bin/benchmark-entrypoint /usr/local/bin/benchmark-push

ENTRYPOINT ["/usr/local/bin/benchmark-entrypoint"]
