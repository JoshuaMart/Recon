FROM golang:1.26.1-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /recon-api .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates curl \
    && curl -fsSL https://github.com/golang-migrate/migrate/releases/download/v4.18.3/migrate.linux-amd64.tar.gz \
       | tar -xz -C /usr/local/bin
COPY --from=builder /recon-api /usr/local/bin/recon-api
COPY db/migrations /migrations
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 3002
ENTRYPOINT ["/entrypoint.sh"]
