# Build any service: docker build --build-arg SERVICE=orders -t k8s-order-saga/orders:dev .
FROM golang:1.27 AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -n "${SERVICE}" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/app ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
# Numeric, so Kubernetes can verify runAsNonRoot (65532 is distroless "nonroot").
USER 65532:65532
ENTRYPOINT ["/app"]
