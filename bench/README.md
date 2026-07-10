# Extraction & fetch benchmarks

Compares **trafilatura** (current extractor) with **Scrapling** on real pages,
both for extraction quality/speed and for fetch success against bot-hostile
sites. Results feed the "fetch smart, extract clean" architecture discussion
in `docs/perf-latency-analysis.md`.

```bash
python -m venv benchenv
benchenv/Scripts/pip install trafilatura lxml_html_clean markdownify "scrapling[fetchers]"
benchenv/Scripts/scrapling install        # only for bench_stealth.py (downloads camoufox)

benchenv/Scripts/python fetch_corpus.py   # downloads the HTML corpus once
benchenv/Scripts/python bench_extractors.py  # extraction: time + content quality
benchenv/Scripts/python bench_fetch.py       # fetch: plain UA vs browser UA vs TLS impersonation
benchenv/Scripts/python bench_stealth.py     # fetch: headless stealth browser on blocked sites
```

Key findings (July 2026, 10-page corpus):

| extractor | median ms/page | boilerplate markers | notes |
|---|---|---|---|
| trafilatura | 125 | 2 | reference quality |
| trafilatura `fast=True` | 86 | 2 | same output on 8/10 pages, extracts less on forum-style pages |
| scrapling text mode | 42 | 29 | ALL visible text: content starts after up to ~7k chars of menus — unusable with the 3000-char answer window |
| scrapling + article/main heuristic | 16 | 7 | starts at the content, but depends on the site's HTML semantics |
| scrapling markdown mode | 140 | 31 | slower than trafilatura and noisy |

Fetching: browser-like headers and Chrome TLS impersonation do **not** get
past JS challenges. The stealth headless browser (`StealthyFetcher` +
`network_idle=True`) does: Le Monde went from a 286-char consent stub to the
full 36,533-char article. Cost: ~8–30 s and a browser process per page (the
JS challenge itself needs several seconds to resolve), plus a ~700 MB
camoufox download — strictly fallback material, never the main fetch path.
Reddit resists even the stealth browser (age-gate/login); it needs
old.reddit.com or the JSON API instead. Beware false "blocks": two of the
"blocked" URLs in the first run were simply dead links (real 404s), and the
block-marker heuristic can match the word "captcha" inside a page's regular
JS.
