"""Fetch benchmark: does the way we download pages change what we can extract?

Methods:
  go_plain       urllib + User-Agent "bap-search/0.1"  — exact mirror of the
                 current Go fetcher (backend/fetch.go)
  go_browser_ua  urllib + full browser-like headers    — the cheap fix that
                 could be done directly in the Go client
  scrapling      scrapling.fetchers.Fetcher.get(impersonate="chrome") — real
                 Chrome TLS fingerprint + stealthy headers (no browser spawned)

Each fetched HTML is then passed through trafilatura.extract to measure what
the PIPELINE would actually get ("combo" architecture: fetch smart, extract
with trafilatura).
"""
import json
import time
import urllib.request

import trafilatura
from scrapling.fetchers import Fetcher

URLS = {
    # struggled or failed with the current fetcher during corpus collection
    "lemonde": "https://www.lemonde.fr/pixels/article/2023/03/28/intelligence-artificielle-des-centaines-d-experts-reclament-une-pause-dans-le-developpement-des-systemes-plus-puissants-que-gpt-4_6167325_4408996.html",
    "arstechnica": "https://arstechnica.com/gadgets/2024/01/what-we-know-about-the-white-houses-right-to-repair-plans/",
    "bbc": "https://www.bbc.com/news/technology",
    # notoriously bot-hostile
    "reddit": "https://www.reddit.com/r/golang/comments/10a9uqi/why_do_people_hate_gorm_so_much/",
    "medium": "https://medium.com/@hnasr/how-slow-is-select-in-postgres-91b4b4a86c36",
    "allrecipes": "https://www.allrecipes.com/recipe/50435/glazed-carrots/",
    "amazon": "https://www.amazon.com/dp/B08N5WRWNW",
    # controls that worked fine with the plain client
    "marmiton": "https://www.marmiton.org/recettes/recette_carottes-vichy_18251.aspx",
    "go_blog": "https://go.dev/blog/loopvar-preview",
}

BROWSER_HEADERS = {
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
    "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
    "Accept-Language": "en-US,en;q=0.8,fr;q=0.6",
}

BLOCK_MARKERS = [
    "captcha", "are you a robot", "access denied", "enable javascript",
    "unusual traffic", "attention required", "just a moment",
    "verify you are a human", "robot check",
]


def classify(status, html, extracted):
    if status >= 400:
        return f"BLOCKED (HTTP {status})"
    low = (html or "")[:20000].lower()
    for marker in BLOCK_MARKERS:
        if marker in low:
            return f"BLOCKED ({marker!r})"
    if len(html or "") < 2048:
        return "STUB (<2KB html)"
    if len(extracted) < 300:
        return "EMPTYISH (<300 chars extracted)"
    return "OK"


def fetch_urllib(url, headers):
    req = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return resp.status, resp.read(2 * 1024 * 1024).decode("utf-8", errors="replace")
    except urllib.error.HTTPError as exc:
        return exc.code, ""
    except Exception as exc:  # noqa: BLE001
        return 0, f"__ERROR__{type(exc).__name__}: {exc}"


def fetch_go_plain(url):
    return fetch_urllib(url, {"User-Agent": "bap-search/0.1"})


def fetch_go_browser_ua(url):
    return fetch_urllib(url, BROWSER_HEADERS)


def fetch_scrapling(url):
    try:
        resp = Fetcher.get(url, impersonate="chrome", stealthy_headers=True, timeout=20, retries=1)
        return resp.status, resp.html_content
    except Exception as exc:  # noqa: BLE001
        return 0, f"__ERROR__{type(exc).__name__}: {exc}"


METHODS = {
    "go_plain": fetch_go_plain,
    "go_browser_ua": fetch_go_browser_ua,
    "scrapling": fetch_scrapling,
}


def main():
    results = {}
    for name, url in URLS.items():
        results[name] = {}
        for method, fn in METHODS.items():
            t0 = time.perf_counter()
            status, html = fn(url)
            elapsed = round((time.perf_counter() - t0) * 1000)
            if html.startswith("__ERROR__"):
                results[name][method] = {"ms": elapsed, "status": status, "verdict": f"ERROR {html[9:90]}", "extracted": 0}
                continue
            try:
                extracted = trafilatura.extract(html) or ""
            except Exception:  # noqa: BLE001
                extracted = ""
            verdict = classify(status, html, extracted)
            results[name][method] = {
                "ms": elapsed,
                "status": status,
                "html_kb": len(html) // 1024,
                "extracted": len(extracted),
                "verdict": verdict,
            }
            time.sleep(0.5)  # be polite between hits on the same site

    json.dump(results, open("bench_fetch_results.json", "w"), indent=2)

    print(f"{'site':<12}" + "".join(f"{m:>42}" for m in METHODS))
    print("-" * (12 + 42 * len(METHODS)))
    for name, row in results.items():
        cells = []
        for method in METHODS:
            r = row[method]
            cells.append(f"{r['verdict'][:26]:>26} {r.get('extracted', 0):>7}ch {r['ms']:>5}ms")
        print(f"{name:<12}" + "".join(f"{c:>42}" for c in cells))

    print("\nOK rate:")
    for method in METHODS:
        ok = sum(1 for row in results.values() if row[method]["verdict"] == "OK")
        total_ms = sum(row[method]["ms"] for row in results.values())
        print(f"  {method:<14} {ok}/{len(results)}   total fetch time {total_ms} ms")


if __name__ == "__main__":
    main()
