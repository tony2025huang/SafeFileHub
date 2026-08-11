# SafeFileHub

SafeFileHub is a Go service for authenticated, permission-scoped file transfer. It uses validated logical paths, opaque object keys, resumable staging uploads, atomic publication, Range downloads, and short-lived archive jobs.

## Release posture

- All paths are logical and validated once; traversal, double encoding, device names, and unsafe prefixes are rejected.
- Objects and staging parts are opened descriptor-relatively with symlink protection.
- Authorization defaults to deny. Downloads hide unauthorized object existence; archive artifacts are private to their creator.
- Uploads are bounded by global, user, and source-IP leases. No unbounded request queue is used.
- Upload completion verifies an optional SHA-256 checksum before publication. Destination conflicts return `409`.

## Build and test

```sh
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -race -count=1
go vet ./...
go test ./... -cover
npm test
npm run build
```

## Container deployment

Build locally:

```sh
docker build -t safefilehub:test .
```

Run with a read-only root filesystem and **one writable data mount**. The image `WORKDIR` is `/var/lib/safefilehub`, so defaults resolve to `/var/lib/safefilehub/data` (objects, `staging`, archive artifacts, and `safefilehub.db`). Staging intentionally remains below the storage root because atomic upload publication requires the same filesystem.

```sh
docker run --read-only --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  --mount type=volume,src=safefilehub-data,dst=/var/lib/safefilehub/data \
  -p 127.0.0.1:8080:8080 safefilehub:test
```

Do not expose the service directly to the internet. Put it behind a TLS-terminating reverse proxy, enforce request-body limits there as well, preserve client IP only from trusted proxies, and restrict the backend listener to a private interface. See [operations](docs/operations.md).

## Operations

- `GET /healthz` is a shallow liveness response; it does not scan storage.
- `GET /readyz` is is part of the production composition root and performs constant-cost database, retained-storage-descriptor, and storage-path checks; it does not scan directories.
- `GET /metrics` exports bounded-label counters. Do not add user paths, contents, passwords, or tokens as labels/log fields.
- Use `deploy/safefilehub.service.example` as a hardened systemd starting point.

This MVP does not include public sharing links, WebDAV, preview rendering, or external deployment automation.
