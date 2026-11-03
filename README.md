# StreamAfrica

Streaming-first African media platform. This repo is the web streaming MVP.
Loop to prove: upload → HLS → discover → watch → pay.

## Prerequisites
- Docker Engine 24+ or Docker Desktop, with **Compose v2.20+** (`docker compose version`)
- `make` and `curl`
- **RAM:** 4 GB allocated to Docker minimum, 6 GB recommended (Next.js dev + FFmpeg image are the heavy parts)
- **Disk:** ~6 GB free for images, volumes and build caches

## Quick start
```bash
cp .env.example .env     # `make up` does this for you if missing
make up                  # builds and starts everything (first run: 2-5 min)
make ps                  # wait until everything is healthy; storage-init exits 0
make smoke               # verifies every endpoint
make migrate-up          # applies all migrations (the database starts empty)
make s3-check            # proves the object store works (multipart, presigned URLs, CORS)
```

## Ports
| Service | URL / port | Notes |
|---|---|---|
| Web (Next.js) | http://localhost:3000 | direct |
| **nginx (front door)** | http://localhost:8088 | `/` web, `/api/*` API, `/hls/*` and `/public/*` media |
| API | http://localhost:8080 | `/healthz`, `/readyz` |
| Object storage (SeaweedFS S3 API) | http://localhost:8333 | keys: `S3_ACCESS_KEY` / `S3_SECRET_KEY`. No web console: use `make storage-ls` |
| Mailpit UI | http://localhost:8025 | SMTP on `localhost:1025` |
| Postgres | localhost:5432 | |
| Redis | localhost:6379 | password from `.env` |
| Worker health | internal only (8081) | |

The table shows the defaults from `.env.example`. Change any `*_HOST_PORT` in the "Host ports" block at the top of `.env` and re-run `make up`; the browser-facing URLs (`NEXT_PUBLIC_*`, `WEB_BASE_URL`, `AUTH_ALLOWED_ORIGINS`) are built from those values automatically. Only the left (host) side of a mapping changes: ports inside the containers (api 8080, web 3000, worker 8081) stay fixed.

All ports bind to `127.0.0.1` by default. Set `BIND_ADDR=0.0.0.0` in `.env` to test from a phone on your LAN.

## Commands
`make help` lists everything: `up`, `down`, `clean` (deletes volumes), `logs s=api`, `ps`, `migrate-up`, `migrate-down` (one step), `test`, `lint`, `fmt`, `smoke`, `s3-check`, `storage-ls`.

## Troubleshooting
**1. "port is already allocated" / "address already in use"**
Find the owner (`lsof -i :8080` or `ss -ltnp | grep 8080`) and stop it, or change the matching `*_HOST_PORT` in `.env` and re-run `make up`. Postgres (5432) and Redis (6379) are the usual clashes with local installs.

**2. Permission errors on volumes or `web/node_modules`, or "permission denied" in SeaweedFS/Postgres**
Containers run as root in dev, so on Linux files they create in bind mounts (`web/node_modules`, `web/.next`, `web/package-lock.json`) are root-owned. Fix with `sudo chown -R "$USER": web api worker`. If a named volume is corrupt, run `make clean` (deletes all local data) and `make up`. On SELinux hosts add `:z` to the bind mounts.

**3. Services killed or restarting (exit code 137), web never becomes healthy**
That is out-of-memory. Raise Docker's memory to 6 GB (Docker Desktop → Settings → Resources), check `docker stats --no-stream`, and close other heavy apps. To start a reduced stack: `docker compose up -d postgres redis seaweedfs storage-init mailpit api`.

**4. SeaweedFS will not start, or "pull access denied ... chrislusf/seaweedfs"**
Run `docker pull chrislusf/seaweedfs:4.51` on its own to see the real error (network, Docker Hub rate limit: `docker login`). If that tag is gone, pick another from Docker Hub and set `SEAWEEDFS_IMAGE` in `.env`, then run `make s3-check` (see `docs/adr/0014-seaweedfs-for-local-object-storage.md`).

**5. Uploads fail with "failed to find writable volumes", or `storage-init` says 403**
The first means SeaweedFS ran out of volume slots: keep `infra/seaweedfs/master.toml` mounted (it limits growth to one volume per bucket). The second means `S3_ACCESS_KEY`/`S3_SECRET_KEY` changed while SeaweedFS was running: `docker compose up -d --force-recreate seaweedfs`.

For any other failure run `make doctor` and read `docs/runbooks/docker-debugging.md`.

## Layout
`web/` Next.js · `api/` Go modular monolith · `worker/` Go FFmpeg worker · `infra/` nginx + scripts · `docs/` architecture, ADRs, API spec, runbooks.
