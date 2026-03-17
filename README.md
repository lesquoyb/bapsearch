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

1. Copy [.env.example](.env.example) to `.env` and fill the OAuth2 Proxy provider values.
2. Put at least one GGUF model into [models](models) or download one from the UI after startup.
3. Start the stack:

```bash
cd docker
docker compose up --build
```

4. Open `http://localhost:8080`.

By default, `http://localhost:8080` goes straight to the backend with anonymous local access enabled.

If you later want authentication, start the optional proxy profile:

```bash
docker compose --profile auth -f docker/docker-compose.yml up --build
```

In authenticated mode, the application is exposed through `auth-proxy`. `backend`, `llama`, and `searxng` stay internal to the Compose network.

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
