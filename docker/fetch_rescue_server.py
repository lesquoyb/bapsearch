#!/usr/bin/env python3
"""Optional stealth fetch sidecar ("fetch-rescue").

The Go backend fetches pages with a plain HTTP client, which JS/anti-bot
walls (Cloudflare, Datadome...) turn into stubs or 403s. When that happens the
backend can POST the URL here; this service loads the page in a stealth
headless browser (Scrapling's camoufox) that resolves the JS challenge and
returns the real HTML — which the backend then runs through trafilatura as
usual ("fetch smart, extract clean").

Heavy on purpose-built resources (a browser per page, seconds per fetch), so
it is only used as a fallback and only ships behind the `fetch-rescue`
compose profile. See bench/README.md for the measurements that motivated it.

API:
  POST /fetch   body: {"url": "https://..."}  ->  200 + raw HTML
  GET  /health                                ->  200 "ok"
"""
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from scrapling.fetchers import StealthyFetcher

MAX_BODY = 64 * 1024
# A browser instance per page is expensive; serialize (or nearly so) the
# rescues instead of letting a burst of blocked pages fork N browsers.
CONCURRENCY = max(1, int(os.environ.get("RESCUE_CONCURRENCY", "1")))
# The JS challenge itself needs several seconds to resolve; network_idle
# waits for it at the cost of also waiting for ad traffic.
TIMEOUT_MS = int(os.environ.get("RESCUE_TIMEOUT_MS", "30000"))

_slots = threading.Semaphore(CONCURRENCY)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _send(self, code, body=b"", content_type="text/plain; charset=utf-8"):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self._send(200, b"ok")
        else:
            self._send(404)

    def do_POST(self):
        if self.path != "/fetch":
            self._send(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            length = 0
        if length <= 0 or length > MAX_BODY:
            self._send(400)
            return
        try:
            payload = json.loads(self.rfile.read(length).decode("utf-8", errors="replace"))
            url = str(payload.get("url", "")).strip()
        except Exception:  # noqa: BLE001
            self._send(400)
            return
        if not url.startswith(("http://", "https://")):
            self._send(400, b"only http(s) urls")
            return

        with _slots:
            try:
                page = StealthyFetcher.fetch(url, headless=True, network_idle=True, timeout=TIMEOUT_MS)
            except Exception as exc:  # noqa: BLE001 - never crash the worker thread
                sys.stderr.write("fetch-rescue error for %s: %s\n" % (url, exc))
                self._send(502, str(exc)[:500].encode("utf-8", errors="replace"))
                return
        if page.status >= 400:
            self._send(502, ("upstream status %d" % page.status).encode())
            return
        html = (page.html_content or "").encode("utf-8", errors="replace")
        if not html:
            self._send(502, b"empty page")
            return
        self._send(200, html, content_type="text/html; charset=utf-8")

    def log_message(self, fmt, *args):
        # One concise line per rescue is useful here (they should be rare).
        sys.stderr.write("fetch-rescue: %s\n" % (fmt % args))


def main():
    host = os.environ.get("RESCUE_HOST", "0.0.0.0")
    port = int(os.environ.get("RESCUE_PORT", "8091"))
    server = ThreadingHTTPServer((host, port), Handler)
    sys.stderr.write("fetch-rescue service listening on %s:%d (concurrency=%d)\n" % (host, port, CONCURRENCY))
    sys.stderr.flush()
    server.serve_forever()


if __name__ == "__main__":
    main()
