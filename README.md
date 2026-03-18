# bap-search

bap-search is a self-hosted conversational search engine designed for small machines. It combines SearXNG for metasearch, a Go backend, a lightweight HTMX UI, SQLite persistence, and llama.cpp for local inference.

## What it does

- Runs SearXNG internally and keeps it off the public network.
- Returns raw search results immediately.
- Fetches the top pages in the background, extracts article text with trafilatura, and summarizes them with a local LLM.
- Stores each search as a conversation thread with messages, results, summaries, and persistent user memory.
- Serves a lightweight interface with HTML templates, HTMX, and minimal JavaScript.
- Logs structured JSON events to a mounted logs volume.

## Stack

- Go 1.24 backend
- SQLite database
- llama.cpp server container
- OAuth2 Proxy for auth enforcement
- Authentik optional self-hosted identity provider
- SearXNG internal metasearch
- Docker Compose orchestration

## LLM runtime

- CPU mode works by default.
- GPU offload is configurable for llama.cpp through Compose env vars.
- NVIDIA mode is enabled with [docker/docker-compose.gpu.yml](docker/docker-compose.gpu.yml) plus a GPU-capable llama.cpp image such as `ghcr.io/ggml-org/llama.cpp:server-cuda`.
- A ready-to-use NVIDIA preset is provided in [.env.nvidia.example](.env.nvidia.example).

## Core flow

1. User submits a search.
2. Backend creates a conversation and stores the initial user message.
3. SearXNG returns raw results immediately.
4. Background workers fetch top pages, run trafilatura, and summarize with llama.cpp.
5. The UI refreshes summaries and lets the user continue in chat mode.
6. User memory is periodically refreshed and reused in later prompts.

## Quick start

1. Copy [.env.example](.env.example) to `.env`.
2. Put at least one GGUF model into [models](models) or download one from the UI after startup.
3. Start the stack:

```bash
cd docker
docker compose up --build
```

4. Open `http://localhost:8080`.

By default, `http://localhost:8080` goes straight to the backend with anonymous local access enabled.

If you later want authentication with an external OIDC provider, start the optional proxy profile:

```bash
docker compose --profile auth -f docker/docker-compose.yml up --build
```

For a production-style auth setup where the backend is not published directly, add [docker/docker-compose.auth-secure.yml](docker/docker-compose.auth-secure.yml):

```bash
docker compose --profile auth -f docker/docker-compose.yml -f docker/docker-compose.auth-secure.yml up --build
```

With this override, only `auth-proxy` is published publicly. `backend`, `llama`, and `searxng` stay internal to the Compose network.

## Authentik

An integrated Authentik stack is included for self-hosted account management.

1. Copy [.env.authentik.example](.env.authentik.example) to `.env`.
	If you launch Compose from [docker](docker), also copy [docker/.env.authentik.example](docker/.env.authentik.example) to [docker/.env](docker/.env), or use `--env-file ../.env.authentik.example`.
2. Start the stack:

```bash
docker compose --profile auth --profile authentik -f docker/docker-compose.yml up --build
```

For a production-style auth setup that hides the backend port completely, use:

```bash
docker compose --profile auth --profile authentik -f docker/docker-compose.yml -f docker/docker-compose.auth-secure.yml up --build
```

3. Open `http://localhost:9000/if/flow/initial-setup/` and finish the initial Authentik setup.
4. Follow [docs/authentik.md](docs/authentik.md) to create the exact Authentik application and provider.
5. Copy the Authentik client ID and client secret into `.env` as `OAUTH2_PROXY_CLIENT_ID` and `OAUTH2_PROXY_CLIENT_SECRET`.
	If you run Compose from [docker](docker), keep the same values in [docker/.env](docker/.env) as well.

`oauth2-proxy` is preconfigured to use the Authentik issuer URL `http://authentik-server:9000/application/o/bap-search/` from inside the Docker network. In the default Authentik preset, `auth-proxy` stays on port `8080`, while the backend is still reachable directly on `8081` for debugging. With [docker/docker-compose.auth-secure.yml](docker/docker-compose.auth-secure.yml), the backend is no longer published at all.

For NVIDIA GPU mode, copy [.env.nvidia.example](.env.nvidia.example) to `.env` and run:

```bash
docker compose -f docker/docker-compose.yml -f docker/docker-compose.gpu.yml up --build
```

## Project layout

- [backend](backend)
- [docker](docker)
- [ui](ui)
- [models](models)
- [database](database)
- [docs/architecture.md](docs/architecture.md)
- [docs/setup.md](docs/setup.md)
- [docs/authentik.md](docs/authentik.md)
- [docs/api.md](docs/api.md)
- [docs/prompts.md](docs/prompts.md)
- [docs/logging.md](docs/logging.md)
- [docs/security.md](docs/security.md)
- [LICENSE](LICENSE)
- [Makefile](Makefile)

## Core endpoints

- `GET /` search landing page
- `POST /search` create conversation and run a search
- `GET /conversations/{id}` conversation view
- `GET /conversations/{id}/summaries` HTMX summary refresh
- `POST /conversations/{id}/messages` follow-up chat
- `GET /models` model management page
- `POST /models/select` select the active GGUF model
- `POST /models/download` download a GGUF file into the shared volume
- `GET /healthz` backend health check

## Notes

- The backend serves the lightweight web UI directly.
- The llama.cpp process is long-lived and reloads only when the selected GGUF model changes.
- The workspace I used here does not have `go` or `docker` installed, so the repository was statically validated but not executed locally.
