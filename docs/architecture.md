# Architecture

## Services

### auth-proxy

- Public entrypoint.
- Uses OAuth2 Proxy.
- Enforces authentication and forwards `X-Forwarded-User` to the backend.

### backend

- Go HTTP service.
- Serves HTML templates and static assets.
- Owns SQLite persistence.
- Calls SearXNG internally with `GET /search?q=<query>&format=json`.
- Orchestrates page fetch, extraction, summarization, conversation storage, and memory refresh.

### searxng

- Internal-only metasearch engine.
- No public port published by Compose.

### llama

- llama.cpp server container.
- Loads the selected GGUF model from `/models/current-model.txt`.
- Watches the selected model file and restarts the inference process when the selected model changes.

## Search workflow

1. User submits a query to `POST /search`.
2. Backend creates a conversation thread and stores the initial user message.
3. Backend queries SearXNG and stores raw results.
4. User is redirected to the conversation page immediately.
5. Background summary workers pick the top distinct-host URLs.
6. Backend downloads HTML pages concurrently.
7. Backend pipes HTML into `trafilatura` and truncates extracted text.
8. Backend summarizes extracted text through llama.cpp.
9. Backend stores summaries and source excerpts in SQLite.
10. HTMX refreshes the summaries block until results appear.

## Conversation flow

- Each search becomes a conversation.
- Follow-up chat uses:
  - persistent user memory
  - stored page summaries
  - extracted source text
  - recent conversation history
- Assistant replies are saved back into the same thread.

## User memory flow

- `user_memory` stores a compact profile per user.
- After enough user turns, the backend sends the latest transcript to the model with:
  - `Update the user memory based on the following conversation.`
- The updated memory summary is stored and injected into future prompts.

## Backend modules

- [backend/main.go](../backend/main.go): config, bootstrap, routes
- [backend/search.go](../backend/search.go): SearXNG client
- [backend/fetch.go](../backend/fetch.go): concurrent fetch and trafilatura extraction
- [backend/summarize.go](../backend/summarize.go): async summary workers
- [backend/conversation.go](../backend/conversation.go): SQLite persistence and chat handlers
- [backend/memory.go](../backend/memory.go): persistent memory refresh
- [backend/logging.go](../backend/logging.go): JSON logging and request context
- [backend/llm.go](../backend/llm.go): llama.cpp chat integration
- [backend/models.go](../backend/models.go): model detection, selection, download

## Database schema

Main tables:

- `users`
- `conversations`
- `messages`
- `search_results`
- `summaries`
- `user_memory`

The canonical schema lives in [database/schema.sql](../database/schema.sql).

## Low-resource design choices

- SQLite instead of a separate database service.
- HTML templates and HTMX instead of a frontend framework.
- Small worker pools for fetch and summarization.
- Only top 3 URLs summarized by default.
- Extracted text truncated before prompting.
- llama.cpp model kept warm in a dedicated container.
- Internal-only search and inference services.
