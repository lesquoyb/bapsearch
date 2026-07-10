"""Fetch a diverse corpus of real pages once, so both extractors run on
identical HTML (mirrors what the Go backend does: it downloads the HTML itself
and POSTs it to the extractor)."""
import json
import pathlib
import urllib.request

URLS = {
    # encyclopedic / reference
    "wikipedia_en": "https://en.wikipedia.org/wiki/Web_scraping",
    "wikipedia_fr": "https://fr.wikipedia.org/wiki/Carotte",
    # news-ish / articles
    "lemonde": "https://www.lemonde.fr/pixels/article/2023/03/28/intelligence-artificielle-des-centaines-d-experts-reclament-une-pause-dans-le-developpement-des-systemes-plus-puissants-que-gpt-4_6167325_4408996.html",
    "bbc": "https://www.bbc.com/news/technology-66196223",
    "arstechnica": "https://arstechnica.com/gadgets/2024/01/what-we-know-about-the-white-houses-right-to-repair-plans/",
    # blogs / docs
    "go_blog": "https://go.dev/blog/loopvar-preview",
    "mdn": "https://developer.mozilla.org/en-US/docs/Web/API/Fetch_API/Using_Fetch",
    "readthedocs": "https://docs.python.org/3/library/http.server.html",
    # forums / QA
    "stackoverflow": "https://stackoverflow.com/questions/11227809/why-is-processing-a-sorted-array-faster-than-processing-an-unsorted-array",
    "hackernews": "https://news.ycombinator.com/item?id=35629127",
    # marketing-heavy page (lots of boilerplate)
    "product_page": "https://www.docker.com/products/docker-desktop/",
    # recipe page (typical bap-search query in tests)
    "marmiton": "https://www.marmiton.org/recettes/recette_carottes-vichy_18251.aspx",
}

out = pathlib.Path("corpus")
out.mkdir(exist_ok=True)
meta = {}
for name, url in URLS.items():
    dest = out / f"{name}.html"
    if dest.exists():
        meta[name] = url
        print(f"cached  {name}")
        continue
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0 (bap-search-bench)"})
        raw = urllib.request.urlopen(req, timeout=20).read()
        dest.write_bytes(raw)
        meta[name] = url
        print(f"fetched {name}: {len(raw)} bytes")
    except Exception as exc:
        print(f"FAILED  {name}: {exc}")

(out / "meta.json").write_text(json.dumps(meta, indent=2))
print(f"\n{len(meta)}/{len(URLS)} pages in corpus")
