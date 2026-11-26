# Build any service: docker build --build-arg SERVICE=orders -t ghcr.io/jmmorell/k8s-order-saga/orders:dev .
# Multi-arch (CI): docker buildx build --platform linux/amd64,linux/arm64 --build-arg SERVICE=orders .
# The build stage runs on the builder's own platform and cross-compiles for the target, so a
# multi-arch build needs no emulation; the final stage has no RUN step.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG SERVICE
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -n "${SERVICE}" && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
  go build -trimpath -ldflags "-s -w" -o /out/app ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
# Numeric, so Kubernetes can verify runAsNonRoot (65532 is distroless "nonroot").
USER 65532:65532
ENTRYPOINT ["/app"]
