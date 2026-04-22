# Build the manager and catalog-builder binaries.
# Base images are pinned by digest so that builds are deterministic and
# supply-chain auditable. To refresh, run:
#   podman pull docker.io/library/golang:1.25 && podman inspect docker.io/library/golang:1.25 --format '{{.Digest}}'
#   podman pull gcr.io/distroless/static:nonroot && podman inspect gcr.io/distroless/static:nonroot --format '{{.Digest}}'
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.25@sha256:4c22673170524e09a47a6e1a5f1ca99692f4796c9b4f2de25ec7137bcd897edd AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY pkg/ pkg/
COPY internal/ internal/

# Build both binaries.
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o catalog-builder ./cmd/catalog-builder/

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:64c43684e6d2b581d1eb362ea47b6a4defee6a9cac5f7ebbda3daa67e8c9b8e6
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/catalog-builder .
USER 65532:65532

ENTRYPOINT ["/manager"]
