# Debugging Docker / Compose errors

Method: read the FIRST error, name its layer, check one fact, change one thing.

1. Find the first real error. Lines marked "Interrupted" are victims: Compose aborts everything in flight when one pull fails.
2. Name the layer: pull (image) -> build (Dockerfile) -> create/start (config, ports, mounts) -> health (app logic) -> dependency order.
3. Gather facts: `make doctor`, `docker compose ps -a`, `docker compose logs --tail=50 <svc>`.
4. Test the smallest piece alone: `docker pull <image>`, `docker compose up <svc>` (no -d), `docker compose run --rm --entrypoint sh <svc>`.
5. Change one thing, rerun the same command, compare output.

Common messages:
- `pull access denied ... repository does not exist` : image name/tag wrong, repo deleted, or private. Run `docker pull <image>` alone.
- `port is already allocated` : change *_HOST_PORT in .env.
- `service "x" is unhealthy` / `dependency failed to start` : `docker compose logs x`, then `docker inspect --format '{{json .State.Health}}' <container>` to see the failing healthcheck output.
- `exited with code 137` : out of memory. `exited with code 1` : app crashed, read logs.
- `no configuration file provided` / empty variables : wrong directory or no .env.
- `missing go.sum entry` : run `make tidy`.
