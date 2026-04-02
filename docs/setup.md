# Setup

## Requirements

- Docker and Docker Compose
- At least one GGUF model placed in [models](../models)
- OAuth provider credentials for OAuth2 Proxy only if you want authenticated access
- Additional CPU and RAM headroom if you run the bundled Authentik stack

## First run

1. Copy [.env.example](../.env.example) to `.env`.
2. Optional for authenticated mode only, set:
   - `OAUTH2_PROXY_PROVIDER`
   - `OAUTH2_PROXY_CLIENT_ID`
   - `OAUTH2_PROXY_CLIENT_SECRET`
   - `OAUTH2_PROXY_OIDC_ISSUER_URL`
   - `OAUTH2_PROXY_COOKIE_SECRET`
   - `OAUTH2_PROXY_REDIRECT_URL`
3. Add a GGUF model into [models](../models) or plan to download one from `/settings` after startup.
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

For a production-style auth setup that does not publish the backend port directly:

```bash
docker compose --profile auth -f docker-compose.yml -f docker-compose.auth-secure.yml up --build
```

### Authentik mode

The repository also includes a self-hosted Authentik deployment. This adds `authentik-server`, `authentik-worker`, PostgreSQL, and Redis.

Recommended path:

1. Copy [.env.authentik.example](../.env.authentik.example) to `.env`.
   If you run commands from [docker](../docker), either copy [docker/.env.authentik.example](../docker/.env.authentik.example) to [docker/.env](../docker/.env) or pass `--env-file ../.env.authentik.example` to `docker compose`.
2. Start the stack:

```bash
docker compose --profile auth --profile authentik up --build
```

For a production-style auth setup that hides the backend completely:

```bash
docker compose --profile auth --profile authentik -f docker-compose.yml -f docker-compose.auth-secure.yml up --build
```

3. Open `http://localhost:9000/if/flow/initial-setup/`.
4. Follow [docs/authentik.md](authentik.md) to create the Authentik application and provider.
5. Copy the generated client ID and secret into `.env`.
   If you run Compose from [docker](../docker), mirror those values into [docker/.env](../docker/.env).

Important details:

- `auth-proxy` listens on `8080`.
- In Authentik mode, set `BAP_PUBLIC_PORT=8081` to avoid a host-port collision with `auth-proxy`.
- In production, use [docker/docker-compose.auth-secure.yml](../docker/docker-compose.auth-secure.yml) so the backend is not published directly.
- The issuer URL used by `oauth2-proxy` is `http://authentik-server:9000/application/o/bap-search/`.
- Authentik itself is exposed on `9000` for HTTP and `9443` for HTTPS by default.

## Volumes and persistent data

- [models](../models): GGUF model files and the role assignment files `current-model.txt`, `current-rewrite-model.txt`, and `current-embedding-model.txt`
- [logs](../logs): JSON backend logs
- [database](../database): SQLite database file

## Model management

- Model files are discovered by scanning `/models` for `.gguf`.
- Saving settings writes the chosen answer, rewrite, and embedding models to role-specific files in `/models`.
- The llama.cpp container polls that file and reloads the model when it changes.

## CPU and GPU modes

Default mode is CPU-only.

Relevant variables in `.env`:

Because bap-search runs 3 separate llama.cpp services, these settings are per-service:

- `LLAMA_ANSWER_IMAGE`, `LLAMA_REWRITE_IMAGE`, `LLAMA_EMBEDDINGS_IMAGE`: llama.cpp container images
- `LLAMA_ANSWER_N_GPU_LAYERS`, `LLAMA_REWRITE_N_GPU_LAYERS`, `LLAMA_EMBEDDINGS_N_GPU_LAYERS`: GPU offload (`0` for CPU-only; `auto` to let llama.cpp fit to VRAM; `all` to try offloading everything; or a number)
- `LLAMA_ANSWER_MAIN_GPU`, `LLAMA_REWRITE_MAIN_GPU`, `LLAMA_EMBEDDINGS_MAIN_GPU`: GPU index used by llama.cpp when multiple GPUs are visible
- `LLAMA_ANSWER_GPUS`, `LLAMA_REWRITE_GPUS`, `LLAMA_EMBEDDINGS_GPUS`: GPU pinning at the Docker level (e.g. `all` or `device=1`)
- `LLAMA_ANSWER_FLASH_ATTN`, `LLAMA_REWRITE_FLASH_ATTN`, `LLAMA_EMBEDDINGS_FLASH_ATTN`: flash attention toggle
- `LLAMA_ANSWER_EXTRA_ARGS`, `LLAMA_REWRITE_EXTRA_ARGS`, `LLAMA_EMBEDDINGS_EXTRA_ARGS`: extra raw llama.cpp server flags

Example NVIDIA setup:

```bash
LLAMA_ANSWER_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda
LLAMA_REWRITE_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda
LLAMA_EMBEDDINGS_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda
LLAMA_ANSWER_N_GPU_LAYERS=auto
LLAMA_REWRITE_N_GPU_LAYERS=auto
LLAMA_EMBEDDINGS_N_GPU_LAYERS=auto
LLAMA_ANSWER_GPUS=all
LLAMA_REWRITE_GPUS=all
LLAMA_EMBEDDINGS_GPUS=all
LLAMA_ANSWER_FLASH_ATTN=true
LLAMA_REWRITE_FLASH_ATTN=true
LLAMA_EMBEDDINGS_FLASH_ATTN=true
docker compose -f docker/docker-compose.yml -f docker/docker-compose.gpu.yml up --build
```

Fastest path:

1. Copy [docker/.env.nvidia.example](../docker/.env.nvidia.example) to `docker/.env`.
2. Start:

```bash
docker compose -f docker/docker-compose.yml -f docker/docker-compose.gpu.yml up --build
```

Notes:

- [docker/docker-compose.gpu.yml](../docker/docker-compose.gpu.yml) enables GPU passthrough for the `llama-answer`, `llama-rewrite`, and `llama-embeddings` services.
- Keep `LLAMA_*_N_GPU_LAYERS=0` if you want CPU-only inference even with a GPU-capable image.
- On Windows, GPU passthrough depends on Docker Desktop, WSL2, and the NVIDIA stack being correctly installed.
- The preset uses `LLAMA_*_N_GPU_LAYERS=auto`, which lets llama.cpp pick an offload that fits your VRAM. Use `all` or an explicit number only if you know your GPU can handle it.

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

See [docs/api.md](api.md) for the full route reference.

## Security notes

- Do not publish `searxng` or `llama` directly.
- Keep `BAP_ALLOW_ANONYMOUS=false` in production.
- Terminate TLS in front of `auth-proxy` if exposed beyond a trusted LAN.
- Rotate OAuth2 Proxy secrets and provider credentials.
- Consider running the stack behind Caddy or Traefik if you need automatic HTTPS.
