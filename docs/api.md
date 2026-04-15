# API

## Browser routes

### GET /

Search landing page. Returns the search form with conversation sidebar.

### POST /search

Creates a conversation and kicks off the search pipeline.

Form field:

- `q`: search query

Behavior: ensures the authenticated user exists, creates a conversation, stores the initial user message, fires SearXNG immediately, enqueues async summarization, and redirects to `/conversations/{id}`.

### GET /conversations/{id}

Conversation view. Returns raw search results, summaries panel, and chat thread.

### GET /conversations/{id}/events

SSE stream for real-time pipeline and card status updates. Sends current state immediately on connect, then pushes incremental events as the pipeline progresses. Event types:

- `pipeline` — overall status (`status`, `detail`, `ready_count`, `target`)
- `card` — per-URL status, detail, source text, and similarity score
- `results` — signals that new search results were stored (client should refresh results panel)
- `close` — pipeline complete; client should reload the messages panel

### GET /conversations/{id}/results

Partial for the raw results block. Loaded once on page load; subsequent updates are driven by SSE events.

### GET /conversations/{id}/summaries

Returns the current state of all summary jobs. Used for initial render only.

### POST /conversations/{id}/summaries/regenerate

Discards existing summaries and re-runs the full fetch, extract, embed, and rerank pipeline from stored search results.

### GET /conversations/{id}/answer/stream

SSE stream for the initial grounded answer. The client connects once and receives the streamed response until completion or a `NEED_MORE_SEARCH` signal.

### GET /conversations/{id}/messages

Returns the messages fragment for HTMX partial refreshes.

### POST /conversations/{id}/messages

Adds a follow-up user message and saves it. The client then connects to `/messages/stream` for the reply.

Form field:

- `message`: follow-up question

### POST /conversations/{id}/messages/stream

SSE stream for an assistant reply to the most recent user message. Uses persistent memory, stored summaries, extracted text, and recent conversation history.

### POST /conversations/{id}/messages/{message_id}/regenerate/stream

SSE stream that regenerates the specified assistant message. Truncates the thread at that message and re-streams.

### POST /conversations/{id}/search-more/stream

SSE stream for an iterative search round. Runs a new SearXNG query, fetches and summarizes new results, then streams an updated answer.

### POST /conversations/{id}/force-answer/stream

SSE stream that forces a grounded answer with the sources available, skipping the `NEED_MORE_SEARCH` check.

### POST /conversations/{id}/delete

Deletes the conversation and all associated messages, results, and summaries.

### GET /settings

Unified settings page. Returns the detected GGUF files, current role assignments, settings form, prompt editors, and model download form.

### POST /settings

Saves the settings form.

Form fields include:

- `llm_model`, `rewrite_model`, `embedding_model`
- LLM sampling parameters (temperature, top_p, top_k, max_tokens)
- Search pipeline parameters (summarize_url_limit, fetch_workers, context_doc_count, etc.)
- Prompt fields (prompt_summarize, prompt_synthesize, prompt_chat, prompt_memory)
- Reasoning settings (enable_thinking, reasoning_budget)

Behavior: writes model assignments to the role-specific files in `/models`, stores remaining settings in SQLite, reloads in-memory prompts and settings.

### POST /settings/download

Downloads a GGUF model file into the shared models volume.

Form field:

- `url`: direct download URL ending in `.gguf`

Behavior: streams the upstream response into `/models/<filename>.part`, atomically renames the file on completion.

### GET /settings/download-status

HTMX partial that returns the current download progress indicator.

### GET /memory

User memory editor page.

### POST /memory

Saves the user memory text.

Form field:

- `memory`: updated memory content

### GET /login

Login form.

### POST /login

Authenticates with username and password. Sets a signed session cookie on success.

### GET /register

Registration form.

### POST /register

Creates a new user account. Accepts username and password.

### POST /logout

Clears the session cookie and redirects to `/login`.

### GET /healthz

Backend health check. Returns `200 ok` when the SQLite database is reachable.

### GET /llama-status

Returns a JSON status object for the specified llama.cpp service.

Query parameter:

- `role`: `answer` or `embeddings`

Response fields:

- `role`, `status` (`loaded`, `loading`, `error`), `expected_model`, `loaded_model`, `detail`

## Internal upstreams

### SearXNG

`GET /search?q=<query>&format=json`

### llama.cpp

`POST /v1/chat/completions` — used for answer generation, query rewriting, summarization, memory refresh.

`POST /v1/embeddings` — used for document and query embedding.

`GET /health`, `GET /v1/models` — polled by `/llama-status`.
