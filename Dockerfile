# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/safefilehub ./cmd/safefilehub

FROM alpine:3.21
RUN addgroup -S safefilehub && adduser -S -G safefilehub -h /var/lib/safefilehub safefilehub \
    && mkdir -p /var/lib/safefilehub/data /var/lib/safefilehub/staging \
    && chown -R safefilehub:safefilehub /var/lib/safefilehub
COPY --from=build /out/safefilehub /usr/local/bin/safefilehub
USER safefilehub:safefilehub
EXPOSE 8080
# Run with --read-only. data (SQLite/objects) and staging are deliberately
# separate writable mounts so staging cleanup cannot affect completed objects.
VOLUME ["/var/lib/safefilehub/data", "/var/lib/safefilehub/staging"]
ENTRYPOINT ["/usr/local/bin/safefilehub"]
