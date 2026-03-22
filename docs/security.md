# Security Strategy

## Authentication

- Authentication is delegated to OAuth2 Proxy.
- The backend does not implement login or session management itself.
- The backend trusts `X-Forwarded-User` only from the authenticated proxy layer.

## Network exposure

- `auth-proxy` is the only published service in Compose.
- `backend`, `searxng`, and `llama` stay on an internal network.
- SearXNG is not exposed publicly.

## Backend trust model

- `BAP_ALLOW_ANONYMOUS=false` should be used in production.
- Only the proxy should be allowed to reach the backend on a real deployment.
- If deployed outside a private LAN, put TLS in front of the public entrypoint.

## Model handling

- Only `.gguf` downloads are accepted by the UI.
- Model downloads use a temporary file and atomic rename.
- Model assignment is file-based and explicit through the role-specific files in `/models`.

## Resource safety

- Fetching and extraction use worker pools to cap concurrency.
- Summaries are limited to a small number of URLs.
- Extracted source text is truncated before inference.
- Context sent to the LLM is bounded.

## Data storage

- SQLite keeps the footprint small but stores conversation history, summaries, and user memory on disk.
- Host permissions on the `database`, `logs`, and `models` directories should be restricted.
- Backups should treat SQLite data and logs as sensitive.

## Deployment recommendations

- Use strong OAuth client credentials and rotate them.
- Use a strong cookie secret for OAuth2 Proxy.
- Run behind Caddy or Traefik if you need automatic HTTPS and safer public exposure.
- Consider egress restrictions if model downloads should only come from trusted hosts.
