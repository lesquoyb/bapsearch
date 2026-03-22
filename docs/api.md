# API

## Public browser routes

### GET /

Search landing page.

Response:

- HTML page with search form
- conversation sidebar
- links to model management

### POST /search

Creates a conversation and performs a SearXNG search.

Form fields:

- `q`: search query

Behavior:

- ensures the authenticated user exists
- creates a conversation row
- stores the initial user message
- calls `GET /search?q=<query>&format=json` on SearXNG
- stores raw search results
- enqueues async summarization
- redirects to `/conversations/{id}`

### GET /conversations/{id}

Conversation page.

Response:

- raw stored search results
- summaries panel
- chat thread
- follow-up chat form

### GET /conversations/{id}/summaries

HTMX partial endpoint for the summaries panel.

Behavior:

- returns only the summaries fragment
- can be polled every few seconds while background work is still running

### POST /conversations/{id}/messages

Adds a user follow-up message and generates an assistant response.

Form fields:

- `message`: follow-up question

Prompt inputs:

- persistent user memory
- summaries
- extracted page text
- recent conversation history
- raw search results as fallback when summaries are not ready yet

Response:

- full page redirect for normal requests
- chat fragment for HTMX requests

### GET /settings

Unified settings page.

Response:

- detected GGUF files
- currently assigned answer, rewrite, and embedding models
- settings form
- prompt editors
- model download form

### POST /settings

Saves the current settings form.

Form fields include:

- `llm_model`, `rewrite_model`, `embedding_model`
- generation and search settings
- prompt fields

Behavior:

- writes model assignments to the role-specific files in `/models`
- stores the remaining settings in SQLite
- reloads in-memory prompts from the database

### POST /settings/download

Downloads a GGUF model file into the shared models volume.

Form fields:

- `url`: direct download URL ending in `.gguf`

Behavior:

- streams the upstream response into `/models/<filename>.part`
- atomically renames the temporary file after completion

### GET /healthz

Simple backend health check.

Checks:

- SQLite connectivity

Response:

- `200 ok` when healthy

## Internal upstreams

### SearXNG

The backend uses:

- `GET /search?q=<query>&format=json`

### llama.cpp

The backend uses:

- `POST /v1/chat/completions`

Expected usage:

- model is already loaded in the llama.cpp service
- backend does not load a model per request
- all inference requests are plain chat-completion style calls
