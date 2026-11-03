# Local dev runbook
- Reset everything: `make clean && make up`
- Tail one service: `make logs s=api`
- API won't start: `docker compose logs api` (config errors list every missing variable)
- Open a DB shell: `docker compose exec postgres psql -U streamafrica`
- Check buckets and sizes: `make storage-ls`
- Prove the object store works (multipart, presigned URLs, CORS, ranges): `make s3-check`
- Changed `S3_ACCESS_KEY`/`S3_SECRET_KEY`: `docker compose up -d --force-recreate seaweedfs`
