# syntax=docker/dockerfile:1.7
# Multi-stage build for every PaymentGateway service.
#
#   docker build -f deploy/docker/Dockerfile --build-arg SERVICE=payment-service -t paymentgateway/payment-service:dev .
#
# Build context MUST be the repository root (go.mod, cmd/, internal/, pkg/, api/, migrations/).
ARG GO_VERSION=1.26
ARG DISTROLESS_IMAGE=gcr.io/distroless/static-debian12:nonroot

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG SERVICE
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG TARGETOS=linux
ARG TARGETARCH

RUN test -n "$SERVICE" || (echo "build-arg SERVICE is required (e.g. --build-arg SERVICE=payment-service)" && exit 1)

RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
# go.su[m] glob tolerates a missing go.sum on a fresh skeleton.
COPY go.mod go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Ensure the migrations directory exists even if this service has none (distroless COPY would otherwise fail).
RUN mkdir -p /src/migrations /out

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT}" \
      -o /out/app ./cmd/${SERVICE}

# ---------------------------------------------------------------------------
# Runtime stage (distroless, non-root, no shell)
# ---------------------------------------------------------------------------
FROM ${DISTROLESS_IMAGE} AS runtime

ARG SERVICE
ARG VERSION=dev
ARG GIT_COMMIT=unknown

LABEL org.opencontainers.image.title="paymentgateway/${SERVICE}" \
      org.opencontainers.image.source="https://github.com/tenghongzou/paymentgateway" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.vendor="PaymentGateway" \
      io.paymentgateway.service="${SERVICE}"

# distroless:nonroot already ships CA certs + tzdata and runs as uid 65532.
COPY --from=build --chown=65532:65532 /out/app /app
# All services' SQL migrations are shipped in every image (PG_AUTO_MIGRATE / `app migrate up` read /migrations/<svc>).
COPY --from=build --chown=65532:65532 /src/migrations /migrations

ENV PG_SERVICE_NAME=${SERVICE} \
    PG_MIGRATIONS_DIR=/migrations \
    TZ=UTC

# HTTP (gateway 8080 / health+metrics 8081) and gRPC (9001-9005, 9101-9102) ports are configured via PG_HTTP_ADDR / PG_GRPC_ADDR.
EXPOSE 8080 8081 9001 9002 9003 9004 9005 9101 9102

USER 65532:65532
ENTRYPOINT ["/app"]
