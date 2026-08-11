# SafeFileHub operations guide

## Deployment boundary

Terminate TLS at a maintained reverse proxy (for example nginx, Caddy, Envoy, or a managed load balancer). Redirect HTTP to HTTPS, use modern TLS defaults, cap request bodies, and pass `X-Forwarded-*` only from explicitly trusted proxy addresses. SafeFileHub intentionally uses the socket peer address until trusted-proxy support is configured; do not trust client-supplied forwarding headers.

Bind SafeFileHub to a private address or loopback behind the proxy. The service is not intended for direct public exposure. Run it as a non-root account with a restrictive umask. `deploy/safefilehub.service.example` sets `ProtectSystem=strict`, a narrow writable path allowlist, and `LimitNOFILE=65536`.

## Storage and backups

Provision separate paths/volumes for:

1. **data** — SQLite metadata and completed opaque objects;
2. **staging** — incomplete resumable upload parts and lifecycle locks.

Use local durable storage. For atomic completion, staging and object storage must be on the same filesystem. Keep permissions private (`0700` directories and `0600` files). Do not browse or publish staging content.

Back up SQLite consistently (use SQLite's supported backup mechanism or a filesystem snapshot coordinated with the service) together with completed object data. Treat this pair as one restore unit. Test restore into an isolated environment: restore data first, verify ownership/modes, start with `-recover-on-start=true`, then run authenticated integrity sampling. Never overwrite production from an untested backup.

## Monitoring and capacity

Alert before the volume is full; a full filesystem must cause writes to fail rather than silently accepting data. Track capacity and inode exhaustion for both data and staging. Monitor:

- `/healthz` availability and latency; `/readyz` dependency failures;
- open file descriptors (`/proc/<pid>/fd`, `lsof`, or service-manager accounting) against `LimitNOFILE`;
- disk free space, inode free space, disk latency/queue depth, SQLite errors, and staging count/age;
- process RSS, goroutines, request 429/503 responses, cancellation and cleanup counters;
- upload/download concurrency and error rate.

Investigate sustained staging growth: it normally means interrupted sessions, a cleanup failure, or insufficient capacity. Use the bounded startup recovery pass and preserve evidence before manual intervention.

For high-bandwidth networks, evaluate BBR and CUBIC **in an isolated benchmark environment** with representative RTT/loss and reverse-proxy TLS. Record throughput, retransmits, CPU, disk wait, FD usage, and health latency. Do not change host sysctls automatically or assume one congestion-control algorithm is universally better.

## Recovery, rollback, and cleanup

On startup, SafeFileHub performs bounded upload reconciliation by default. For a no-write inspection:

```sh
safefilehub -recover-on-start=true -recover-only -recover-dry-run -recover-limit=64
```

For an actual bounded cleanup after reviewing monitoring and backup state:

```sh
safefilehub -recover-on-start=true -recover-only -recover-limit=64
```

Do not delete staging directories with broad shell commands. Recovery validates session metadata, regular files, offsets, expiry, and lifecycle locks; it removes only safe orphans/expired parts within its configured limit.

Rollback procedure:

1. Drain or stop new traffic at the reverse proxy.
2. Keep the current binary/image and data volumes intact; collect health, disk, and application logs without secrets.
3. Deploy the prior tested binary/image and restart gracefully.
4. Run bounded recovery, verify `/healthz`, authenticated listing/download, and a checksum-verified resumed upload.
5. If metadata/object consistency is in doubt, restore the matched SQLite/object backup pair to an isolated environment first. Escalate rather than deleting objects or staging parts manually.

## Release gate

Before a release, run:

```sh
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -race -count=1
go vet ./...
go test ./... -cover
npm test
npm run build
git diff --check
docker build -t safefilehub:test .
```

The stress suite covers 1/2/4/8/16 concurrent files, health responsiveness, and SHA-256 integrity. Run destructive disk-full and external-network exercises only in isolated disposable test environments; never fill a production filesystem to test error handling.

## Bootstrap, logs, and reverse proxies

First startup logs a one-time random initial-admin credential pair. Capture it from the protected service log. It is not stored in database audit data. Use `--reset-initial-admin` only as an explicit break-glass operation; it resets id 1 and exits before HTTP starts.

Keep persistent `data/` on a durable volume (never tmpfs in production). Configure structured logs with JSON where possible and restrict access to the log file because the one-time bootstrap credential appears there. Request logs contain request IDs, client and peer addresses, status, byte count, and duration, but redact credentials, cookies, authorization values, content, and sensitive query data.

Nginx must pass its direct peer address and append forwarding information:
```
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
```
Set `--trusted-proxy-cidr` exclusively to the CIDR(s) from which Nginx connects. Otherwise SafeFileHub uses the direct peer and ignores spoofable headers.


## File lifecycle API boundary

Files become published only when an upload session is completed. A directory upload
is represented as multiple upload sessions, one for each file. SafeFileHub does
**not** expose standalone APIs to create or delete already-published files or
directories. `DELETE /api/uploads/{id}` cancels an incomplete upload session and
must not be interpreted as deleting a published file.

## Audit event coverage

Each request has one terminal structured request event, including login, logout,
file listing, administration, and denied requests. Upload, download, and archive
handlers additionally emit explicit `*.start` before handler execution and
`*.complete` at success, failure, or cancellation. These lifecycle records carry
request/session/transfer correlation IDs, resolved client and peer IPs, status,
success, byte count, and duration; credentials, cookies, authorization data,
request content, and sensitive query data are redacted.

## Published object mutation API

Published files are created **only** through the resumable upload completion API;
there is deliberately no empty-file endpoint because every published `files` row
must reference an immutable opaque object. Explicit logical directories are
created with `POST /api/directories` body `{"root_id": 1, "path":"reports"}`
and return `201` with `id`, `root_id`, and canonical `path`.

`DELETE /api/files/{fileID}` removes one completed opaque object and then its
metadata, requiring `delete` on that exact logical path. `DELETE
/api/directories/{directoryID}` requires `delete` on the directory and is
non-recursive: it returns `409` if any published file or explicit child
directory exists. Both reject unauthenticated and unauthorized requests.
`DELETE /api/uploads/{id}` remains only cancellation of an incomplete staging
upload and never deletes a published file.

The structured log matrix includes `directory.create.start|complete`,
`file.delete.start|complete`, and `directory.delete.start|complete`, plus their
terminal request records. Each includes operation, success/status, client_ip,
peer_ip, request_id, and session_audit_id; request content, cookies, tokens and
other sensitive fields remain excluded.
