# Built from the repository root: docker build -f deploy/compose/Dockerfile .
FROM node:24-alpine AS web
WORKDIR /web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The interface the binary embeds.
COPY --from=web /web/build/ internal/ui/dist/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -X github.com/JoshuaMart/recon/internal/version.Version=${VERSION}" \
	-o /out/recon ./cmd/recon
# The screenshot directory, owned by the runtime user: a named volume mounted
# there takes this ownership on first use.
RUN mkdir -p /out/data/screenshots

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/recon /recon
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8080
ENTRYPOINT ["/recon"]
CMD ["serve"]
