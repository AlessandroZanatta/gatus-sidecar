FROM golang:1.26-alpine AS build

# Root certificates are the one thing the binary cannot supply itself. In-cluster
# it uses the service account CA bundle, but running against a kubeconfig whose
# API server has a public certificate needs the system store.
RUN apk add --no-cache ca-certificates

WORKDIR /src

# Dependencies are copied first so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Static, so the result needs nothing from the base image.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
# An image reporting "dev" was not built by the release pipeline.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/gatus-sidecar ./cmd/gatus-sidecar

# No USER is set: the sidecar shares a volume with Gatus and has to write a file
# Gatus can read, so the pod's securityContext decides the uid and fsGroup.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gatus-sidecar /gatus-sidecar
ENTRYPOINT ["/gatus-sidecar"]
