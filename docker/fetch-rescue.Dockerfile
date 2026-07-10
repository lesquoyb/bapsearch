# Optional stealth-fetch sidecar (compose profile: fetch-rescue).
# Ships a headless stealth browser (camoufox via Scrapling), so the image is
# large (~2.2 GB built) — that's why it's opt-in.
FROM python:3.12-slim

# System libraries camoufox (Firefox-based) needs to run headless.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        libgtk-3-0 libx11-xcb1 libdbus-glib-1-2 libxt6 libpci3 \
        libasound2 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
        libgbm1 libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 \
        libdrm2 libxkbcommon0 libepoxy0 fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir "scrapling[fetchers]" \
    && scrapling install

COPY fetch_rescue_server.py /app/fetch_rescue_server.py

EXPOSE 8091
CMD ["python3", "/app/fetch_rescue_server.py"]
