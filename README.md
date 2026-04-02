# bap-search

Self-hosted conversational search engine for small machines. Combines SearXNG metasearch, a Go backend, llama.cpp for local inference, SQLite persistence, and a lightweight HTMX interface.

## What it does

- Runs SearXNG internally on an isolated Docker network.
- Returns raw search results immediately while the pipeline works in the background.
- Rewrites the user query with a small LLM and runs both the original and rewritten queries against SearXNG in parallel.
- Fetches each result page, extracts clean text with trafilatura, generates embeddings, and reranks sources by cosine similarity.
- Streams a grounded answer from the top-ranked sources with inline citations.
- Supports iterative search: the model can request additional searches when it needs more information, with user confirmation.
- Stores everything as threaded conversations with messages, results, summaries, and engine statuses.
- Maintains persistent per-user memory that is automatically refreshed and reused across conversations.
- Supports chain-of-thought reasoning with configurable budget (for models like Qwen3.5).
- All parameters (LLM sampling, search depth, context limits, prompts) are editable from the settings page.
- Logs structured JSON to a mounted volume.

## Stack

| Component | Role |
|---|---|
| Go 1.25 backend | API, search pipeline, streaming, SQLite |
| SQLite | Conversations, users, settings, memory |
| llama.cpp (×3 containers) | Answer, query rewrite, embeddings |
| SearXNG | Internal metasearch engine |
| trafilatura | Web page text extraction |
| HTMX + vanilla JS | Lightweight frontend |
| Docker Compose | Orchestration |

## Quick start

1. Put at least one `.gguf` model into the `models/` directory (or download one from the settings page after startup).

2. Start the stack:

   ```bash
   cd docker
   docker compose up --build
   ```

3. Open http://localhost:8080.

By default, anonymous access is enabled (`BAP_ALLOW_ANONYMOUS=true`) and you land directly on the search page as `dev-user`.

With `make`:

```bash
make up        # CPU mode
make up-gpu    # NVIDIA GPU mode
```

### NVIDIA GPU mode

Copy `docker/.env.nvidia.example` to `docker/.env` then:

```bash
docker compose -f docker/docker-compose.yml -f docker/docker-compose.gpu.yml up --build
```

This uses `ghcr.io/ggml-org/llama.cpp:server-cuda` with full GPU layer offload.

## Search pipeline

1. User submits a query.
2. Backend creates a conversation and enqueues a background job.
3. The original query goes to SearXNG. A rewrite model produces an optimized search query in parallel.
4. Both result sets are merged and displayed as raw results.
5. Top URLs are fetched concurrently (`BAP_FETCH_WORKERS`), cleaned with trafilatura, and embedded.
6. Sources are reranked by cosine similarity against the rewritten-query embedding.
7. The answer model streams a grounded response with citations from the top sources.
8. If the model signals it needs more data (`NEED_MORE_SEARCH`), the user is prompted to continue searching or answer with what's available.
9. User memory is refreshed in the background after each conversation.

## Multi-model architecture

Three dedicated llama.cpp services run in parallel, each watching its own model file:

| Service | Env var | Model file |
|---|---|---|
| `llama-answer` | `LLAMA_CPP_URL` | `models/current-model.txt` |
| `llama-rewrite` | `LLAMA_CPP_REWRITE_URL` | `models/current-rewrite-model.txt` |
| `llama-embeddings` | `LLAMA_CPP_EMBEDDINGS_URL` | `models/current-embedding-model.txt` |

Model assignments are managed from the settings page. Each file contains the filename of the `.gguf` to load. If a file is missing, the service falls back to the first `.gguf` found in `models/`.

The backend truncates document text before embedding using `BAP_MAX_EMBEDDING_CHARS` to avoid exceeding llama.cpp batch limits.

## Authentication

bap-search supports three authentication modes, from simplest to most secure:

### 1. Anonymous mode (default)

No login required. All requests use `dev-user`.

```env
BAP_ALLOW_ANONYMOUS=true
```

### 2. Embedded accounts

Built-in username/password authentication with bcrypt-hashed passwords stored in SQLite. No external service needed.

```env
BAP_ALLOW_ANONYMOUS=false
BAP_SESSION_SECRET=your-random-secret-here
```

- Users register at `/register` and sign in at `/login`.
- Sessions are HMAC-SHA256 signed cookies valid for 30 days.
- If `BAP_SESSION_SECRET` is not set, a random one is generated at startup (sessions won't survive restarts).
- The logout button appears in the sidebar.

### 3. External OIDC (OAuth2 Proxy + Authentik)

For production deployments with SSO. Uses OAuth2 Proxy in front of the backend with an external OIDC provider.

```bash
docker compose --profile auth -f docker/docker-compose.yml up --build
```

To also run a self-hosted Authentik identity provider:

```bash
docker compose --profile auth --profile authentik -f docker/docker-compose.yml up --build
```

To hide the backend port entirely in production:

```bash
docker compose --profile auth --profile authentik \
  -f docker/docker-compose.yml \
  -f docker/docker-compose.auth-secure.yml up --build
```

See [docs/authentik.md](docs/authentik.md) for the Authentik provider setup.

**Authentication priority:** `X-Forwarded-User` header (proxy) → session cookie (embedded) → anonymous fallback → redirect to `/login`.

## Configuration

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `BAP_ADDR` | `:8081` | Backend listen address |
| `BAP_ALLOW_ANONYMOUS` | `true` | Allow unauthenticated access |
| `BAP_SESSION_SECRET` | (auto-generated) | HMAC key for session cookies |
| `BAP_DB_PATH` | `/database/bap-search.db` | SQLite database path |
| `BAP_SUMMARIZE_URL_LIMIT` | `3` | URLs to fetch & summarize per search |
| `BAP_FETCH_WORKERS` | `3` | Concurrent page fetch workers |
| `BAP_MAX_EXTRACT_CHARS` | `12000` | Max chars extracted per page |
| `BAP_MAX_EMBEDDING_CHARS` | `1800` | Max chars sent to embedding model |
| `BAP_CHAT_CONTEXT_CHARS` | `4200` | Conversation context for follow-ups |
| `BAP_MAX_CHAT_MESSAGES` | `8` | Max messages in chat context |
| `BAP_SUMMARY_WORKERS` | `1` | Concurrent summary pipeline workers |
| `BAP_CONTEXT_DOC_COUNT` | `5` | Top-ranked sources included in answer context |
| `BAP_LLM_MAX_TOKENS` | `700` | Max response tokens for utility tasks |
| `BAP_LLM_CONTEXT_TOKENS` | `8192` | LLM context window size |
| `LLAMA_CPP_URL` | `http://llama-answer:8080/v1/chat/completions` | Answer model endpoint |
| `LLAMA_CPP_REWRITE_URL` | (same as answer) | Rewrite model endpoint |
| `LLAMA_CPP_EMBEDDINGS_URL` | `http://llama:8080/v1/embeddings` | Embedding model endpoint |

### UI settings

All of these are adjustable from the `/settings` page without restart:

- **Model assignments** — answer, rewrite, embedding model per role
- **LLM sampling** — temperature, top-p, top-k, max tokens
- **Reasoning** — enable/disable chain-of-thought, reasoning budget
- **Search** — results per search, iterative search loops, URLs to summarize, max extract chars, fetch workers
- **Embeddings** — similarity threshold
- **Chat** — context chars, max messages in context
- **Prompts** — summarize, synthesize, chat, and memory prompts (fully editable)

## Core endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Search landing page |
| `POST` | `/search` | Create conversation and run search |
| `GET` | `/conversations/{id}` | Conversation view |
| `GET` | `/conversations/{id}/results` | HTMX raw results refresh |
| `GET` | `/conversations/{id}/summaries` | HTMX summary refresh |
| `GET` | `/conversations/{id}/answer/stream` | SSE initial answer stream |
| `POST` | `/conversations/{id}/messages` | Follow-up chat message |
| `POST` | `/conversations/{id}/messages/stream` | SSE chat reply stream |
| `POST` | `/conversations/{id}/search-more/stream` | SSE iterative search stream |
| `POST` | `/conversations/{id}/force-answer/stream` | SSE force answer with current sources |
| `POST` | `/conversations/{id}/summaries/regenerate` | Rebuild summaries from stored results |
| `POST` | `/conversations/{id}/delete` | Delete conversation |
| `GET` | `/settings` | Settings page |
| `POST` | `/settings` | Save settings |
| `POST` | `/settings/download` | Download a GGUF model from URL |
| `GET` | `/memory` | View/edit persistent user memory |
| `POST` | `/memory` | Save user memory |
| `GET` | `/login` | Login page |
| `POST` | `/login` | Authenticate |
| `GET` | `/register` | Registration page |
| `POST` | `/register` | Create account |
| `POST` | `/logout` | Sign out |
| `GET` | `/healthz` | Health check |
| `GET` | `/llama-status` | Model server status (JSON) |

## Project layout

```
backend/          Go backend (single binary)
database/         SQLite schema
docker/           Compose files, Dockerfiles, SearXNG config
  backend.Dockerfile
  docker-compose.yml
  docker-compose.gpu.yml
  docker-compose.auth-secure.yml
  llama-entrypoint.sh
  searxng-settings.yml
  .env.example
  .env.nvidia.example
  .env.authentik.example
ui/
  templates/      HTML templates (layout, index, conversation, settings, memory, login, register)
  static/         CSS, favicon
models/           GGUF model files + current-model pointers
logs/             Structured JSON logs
docs/             Architecture, setup, auth, API, prompts, logging, security docs
Makefile          Shortcuts for compose commands
```

## License

See [LICENSE](LICENSE).
