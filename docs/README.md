# Documentation

| File | What it is |
|---|---|
| [`openapi.yaml`](openapi.yaml) | The HTTP API contract. CI fails if handlers and this file disagree. Also served as an interactive page: **http://localhost:8088/api/docs/** (run `make docs`). |
| [`api-examples.md`](api-examples.md) | curl examples with real responses for every catalog, library and sign-in flow. |
| [`data-model.md`](data-model.md) | ER diagram, one line per table, indexes and EXPLAIN evidence. |
| [`security-auth.md`](security-auth.md) | Authentication threat model: what is defended, how it is tested, what remains open. |
| [`adr/`](adr/README.md) | Architecture decision records. |
| [`architecture.md`](architecture.md) | Components and environments. |
| [`runbooks/`](runbooks/) | Operating notes. |
