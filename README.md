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
make ps                  # wait until everything is healthy; minio-init exits 0
make smoke               # verifies every endpoint
make migrate-up          # applies the baseline migration
```

## Ports
| Service | URL / port | Notes |
|---|---|---|
| Web (Next.js) | http://localhost:3000 | direct |
| **nginx (front door)** | http://localhost:8088 | `/` web, `/api/*` API, `/hls/*` and `/public/*` media |
| API | http://localhost:8080 | `/healthz`, `/readyz` |
| MinIO S3 API | http://localhost:9000 | |
| MinIO console | http://localhost:9001 | login: `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` |
| Mailpit UI | http://localhost:8025 | SMTP on `localhost:1025` |
| Postgres | localhost:5432 | |
| Redis | localhost:6379 | password from `.env` |
| Worker health | internal only (8081) | |

All ports bind to `127.0.0.1` by default. Set `BIND_ADDR=0.0.0.0` in `.env` to test from a phone on your LAN.

## Commands
`make help` lists everything: `up`, `down`, `clean` (deletes volumes), `logs s=api`, `ps`, `migrate-up`, `migrate-down` (one step), `test`, `lint`, `fmt`, `smoke`.

## Troubleshooting
**1. "port is already allocated" / "address already in use"**
Find the owner (`lsof -i :8080` or `ss -ltnp | grep 8080`) and stop it, or change the matching `*_HOST_PORT` in `.env` and re-run `make up`. Postgres (5432) and Redis (6379) are the usual clashes with local installs.

**2. Permission errors on volumes or `web/node_modules`, or "permission denied" in MinIO/Postgres**
Containers run as root in dev, so on Linux files they create in bind mounts (`web/node_modules`, `web/.next`, `web/package-lock.json`) are root-owned. Fix with `sudo chown -R "$USER": web api worker`. If a named volume is corrupt, run `make clean` (deletes all local data) and `make up`. On SELinux hosts add `:z` to the bind mounts.

**3. Services killed or restarting (exit code 137), web never becomes healthy**
That is out-of-memory. Raise Docker's memory to 6 GB (Docker Desktop → Settings → Resources), check `docker stats --no-stream`, and close other heavy apps. To start a reduced stack: `docker compose up -d postgres redis minio minio-init mailpit api`.

## Layout
`web/` Next.js · `api/` Go modular monolith · `worker/` Go FFmpeg worker · `infra/` nginx + scripts · `docs/` architecture, ADRs, API spec, runbooks.
