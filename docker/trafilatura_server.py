#!/usr/bin/env python3
"""Long-lived trafilatura extraction service.

The Go backend used to spawn the `trafilatura` CLI once per URL, paying the
Python interpreter + library import cost (hundreds of ms to >1s) every time.
This service imports trafilatura once at startup and serves extraction over
HTTP, so each page costs only the extraction itself.

Stdlib only (no Flask). POST the raw HTML to /extract and get the extracted
main text back as the body. GET /health for readiness.
"""
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import trafilatura

MAX_BYTES = 16 * 1024 * 1024  # reject absurdly large bodies

# fast=True skips trafilatura's fallback extraction algorithms: ~30% faster
# with identical output on most pages (see bench/README.md); forum-style pages
# may extract a bit less. Set TRAFILATURA_FAST=false to restore full mode.
FAST = os.environ.get("TRAFILATURA_FAST", "true").strip().lower() in ("1", "true", "yes", "on")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"  # keep connections alive across pages

    def _send(self, code, body=b""):
        self.send_response(code)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
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
        if self.path != "/extract":
            self._send(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            length = 0
        if length <= 0 or length > MAX_BYTES:
            self._send(400)
            return
        raw = self.rfile.read(length)
        try:
            html = raw.decode("utf-8", errors="replace")
            # Mirror the CLI default (main text, txt output).
            text = trafilatura.extract(html, fast=FAST) or ""
            if not text and FAST:
                # The fast path found nothing: give the fallback algorithms a
                # chance before declaring the page empty.
                text = trafilatura.extract(html) or ""
        except Exception as exc:  # noqa: BLE001 - never crash the worker thread
            sys.stderr.write("trafilatura extract error: %s\n" % exc)
            self._send(500)
            return
        self._send(200, text.encode("utf-8"))

    def log_message(self, *args):  # keep stderr quiet (one log line per request is noise)
        pass


def main():
    host = os.environ.get("TRAFILATURA_HOST", "127.0.0.1")
    port = int(os.environ.get("TRAFILATURA_PORT", "8090"))
    server = ThreadingHTTPServer((host, port), Handler)
    sys.stderr.write("trafilatura service listening on %s:%d\n" % (host, port))
    sys.stderr.flush()
    server.serve_forever()


if __name__ == "__main__":
    main()
