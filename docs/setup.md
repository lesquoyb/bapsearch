# Setup

## Requirements

- Docker and Docker Compose
- At least one GGUF model placed in [models](../models)
- OAuth provider credentials for OAuth2 Proxy only if you want authenticated access

## First run

1. Copy [.env.example](../.env.example) to `.env`.
2. Optional for authenticated mode only, set:
   - `OAUTH2_PROXY_PROVIDER`
   - `OAUTH2_PROXY_CLIENT_ID`
   - `OAUTH2_PROXY_CLIENT_SECRET`
   - `OAUTH2_PROXY_COOKIE_SECRET`
   - `OAUTH2_PROXY_REDIRECT_URL`
3. Add a GGUF model into [models](../models) or plan to download one from `/models` after startup.
4. Start the stack from [docker](../docker):

```bash
docker compose up --build
```

5. Open `http://localhost:8080`.

This default mode bypasses OAuth and talks directly to the backend.

## Authenticated mode

If you later want to enable `oauth2-proxy`, provide real OAuth credentials in `.env` and start Compose with the `auth` profile:

```bash
docker compose --profile auth up --build
```

## Volumes and persistent data

- [models](../models): GGUF model files and `current-model.txt`
- [logs](../logs): JSON backend logs
- [database](../database): SQLite database file

## Model management

- Model files are discovered by scanning `/models` for `.gguf`.
- Selecting a model writes the chosen file name to `/models/current-model.txt`.
- The llama.cpp container polls that file and reloads the model when it changes.

## CPU and GPU modes

Default mode is CPU-only.

Relevant variables in `.env`:

- `LLAMA_IMAGE`: llama.cpp container image
- `LLAMA_N_GPU_LAYERS`: number of layers offloaded to GPU, `0` keeps CPU-only mode
- `LLAMA_MAIN_GPU`: GPU index used by llama.cpp
- `LLAMA_FLASH_ATTN`: enables flash attention when supported by the image/backend
- `LLAMA_EXTRA_ARGS`: extra raw llama.cpp server flags

Example NVIDIA setup:

```bash
LLAMA_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda
LLAMA_N_GPU_LAYERS=999
LLAMA_FLASH_ATTN=true
docker compose -f docker/docker-compose.yml -f docker/docker-compose.gpu.yml up --build
```

Fastest path:

1. Copy [.env.nvidia.example](../.env.nvidia.example) to `.env`.
2. Start:

```bash
docker compose -f docker/docker-compose.yml -f docker/docker-compose.gpu.yml up --build
```

Notes:

- [docker/docker-compose.gpu.yml](../docker/docker-compose.gpu.yml) enables `gpus: all` for the `llama` service.
- Keep `LLAMA_N_GPU_LAYERS=0` if you want CPU-only inference even with a GPU-capable image.
- On Windows, GPU passthrough depends on Docker Desktop, WSL2, and the NVIDIA stack being correctly installed.
- The preset uses `LLAMA_N_GPU_LAYERS=999`, which asks llama.cpp to offload as much as possible. Lower it if VRAM is insufficient.

## Development notes

- Backend port inside Compose: `8081`
- Default local entrypoint: `http://localhost:8080`
- Optional OAuth-protected proxy entrypoint: `http://localhost:8080` when the `auth` profile is enabled
- SearXNG and llama.cpp are internal-only services.
- Local development defaults to `BAP_ALLOW_ANONYMOUS=true`.
- Without the `auth` profile, no external authentication service is required.

## Logging

The backend writes structured JSON logs to `/logs/backend.jsonl` and stdout. Log records include:

- timestamp
- request_id
- user_id
- conversation_id
- search queries
- fetched URLs
- extracted text size
- LLM prompts
- LLM responses
- errors

## API surface

- `GET /` search landing page
- `POST /search` search workflow entrypoint
- `GET /conversations/{id}` conversation page
- `GET /conversations/{id}/summaries` async summaries block
- `POST /conversations/{id}/messages` follow-up chat
- `GET /models` model page
- `POST /models/select` select active model
- `POST /models/download` direct GGUF download
- `GET /healthz` health check

## Security notes

- Do not publish `searxng` or `llama` directly.
- Keep `BAP_ALLOW_ANONYMOUS=false` in production.
- Terminate TLS in front of `auth-proxy` if exposed beyond a trusted LAN.
- Rotate OAuth2 Proxy secrets and provider credentials.
- Consider running the stack behind Caddy or Traefik if you need automatic HTTPS.
