# Build the manager binary.
#
# This Dockerfile must also build under the legacy docker builder that Google Cloud Build's
# gcr.io/cloud-builders/docker step uses. So it carries no BuildKit-only syntax: no RUN --mount
# cache, and no --platform=$BUILDPLATFORM. The binary is built for the host platform.
FROM golang:1.27 AS builder

WORKDIR /workspace

# Copy the Go module manifests first, so a source change does not re-download the dependencies.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the Go source. .dockerignore filters what does not belong in the image.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -o manager cmd/main.go

# Distroless keeps the image to the binary and its certificates. It carries no shell, so the
# container reads its settings from DVC_ environment variables and nothing else.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
