# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ENV CGO_ENABLED=0
ENV GOOS=linux
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    sh -eu -c 'for bin in api worker sweeper scheduler oauth-setup; do \
      go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/$bin ./cmd/$bin ; \
    done'

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/worker /out/sweeper /out/scheduler /out/oauth-setup /
EXPOSE 8080
USER nonroot:nonroot
# Default entrypoint is api; override per-deployment via k8s command: for workers, sweeper, etc.
ENTRYPOINT ["/api"]
