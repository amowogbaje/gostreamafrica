# Local dev runbook
- Reset everything: `make clean && make up`
- Tail one service: `make logs s=api`
- API won't start: `docker compose logs api` (config errors list every missing variable)
- Open a DB shell: `docker compose exec postgres psql -U streamafrica`
- Check buckets: MinIO console at http://localhost:9001
