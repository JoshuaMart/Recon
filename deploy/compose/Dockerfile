# One build, several entrypoints. Each target is a distroless image carrying a
# single static binary, so a compromised process finds no shell to use.

FROM golang:1.26-alpine AS builder
WORKDIR /build

# Cached separately from the source, so editing code does not re-download the
# module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/controlplane ./cmd/controlplane && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot AS controlplane
COPY --from=builder /out/controlplane /controlplane
EXPOSE 8080
ENTRYPOINT ["/controlplane"]

FROM gcr.io/distroless/static-debian12:nonroot AS migrate
COPY --from=builder /out/migrate /migrate
ENTRYPOINT ["/migrate"]
