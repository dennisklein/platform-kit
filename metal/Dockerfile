# The operator image: the manager, and the pk-metal CLI next to it so that a
# `kubectl exec` or a Job has the same verbs a workstation has, at the same
# version. Both are static binaries on distroless/static: no shell, no package
# manager, a non-root user, CA certificates and tzdata and nothing else.
#
# Build it with `mise run image`, which tags it with the tree it was built from.
#
# The minor of each base is pinned and the patch floats, like the toolchain in
# mise.toml. distroless publishes no versioned tags, so that one floats within
# its Debian release.
FROM golang:1.26 AS build
ARG VERSION=dev
WORKDIR /src
# The image's own Go, never a downloaded one: a toolchain fetch inside a build
# is a second, unpinned base image.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY . .
# The caches are BuildKit mounts, so the layer above can change on every commit
# without refetching modules or recompiling the dependency tree. The secret is
# optional and for one case only: a network that re-signs TLS (a corporate or
# CI proxy) needs its CA bundle while modules download, and that bundle must
# not end up in a layer.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=secret,id=ca-bundle,target=/etc/ssl/certs/ca-certificates.crt,required=false \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/manager ./cmd/pk-metal

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/manager /out/pk-metal /
# distroless/static:nonroot already runs as 65532; stating it keeps the image
# honest if the base ever changes its default, and lets a runAsNonRoot check
# see a numeric user.
USER 65532:65532
ENTRYPOINT ["/manager"]
