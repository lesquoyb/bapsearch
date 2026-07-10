"""StealthyFetcher (camoufox headless) on the sites that blocked HTTP-level
fetching, then trafilatura on top — the 'fetch smart, extract clean' combo."""
import json
import time

import trafilatura
from scrapling.fetchers import StealthyFetcher

URLS = {
    "lemonde": "https://www.lemonde.fr/pixels/article/2023/03/28/intelligence-artificielle-des-centaines-d-experts-reclament-une-pause-dans-le-developpement-des-systemes-plus-puissants-que-gpt-4_6167325_4408996.html",
    "arstechnica": "https://arstechnica.com/gadgets/2024/01/what-we-know-about-the-white-houses-right-to-repair-plans/",
    "reddit": "https://www.reddit.com/r/golang/comments/10a9uqi/why_do_people_hate_gorm_so_much/",
    "amazon": "https://www.amazon.com/dp/B08N5WRWNW",
    "marmiton": "https://www.marmiton.org/recettes/recette_carottes-vichy_18251.aspx",  # control
}

BLOCK_MARKERS = ["captcha", "are you a robot", "access denied", "enable javascript",
                 "unusual traffic", "attention required", "just a moment", "verify you are a human"]

results = {}
for name, url in URLS.items():
    t0 = time.perf_counter()
    try:
        resp = StealthyFetcher.fetch(url, headless=True, timeout=45000, network_idle=True)
        html = resp.html_content
        status = resp.status
    except Exception as exc:  # noqa: BLE001
        results[name] = {"verdict": f"ERROR {type(exc).__name__}: {str(exc)[:80]}", "ms": round((time.perf_counter() - t0) * 1000)}
        continue
    elapsed = round((time.perf_counter() - t0) * 1000)
    extracted = ""
    try:
        extracted = trafilatura.extract(html) or ""
    except Exception:  # noqa: BLE001
        pass
    low = (html or "")[:20000].lower()
    verdict = "OK"
    if status >= 400:
        verdict = f"BLOCKED (HTTP {status})"
    else:
        for m in BLOCK_MARKERS:
            if m in low:
                verdict = f"BLOCKED ({m!r})"
                break
        else:
            if len(extracted) < 300:
                verdict = "EMPTYISH"
    results[name] = {"verdict": verdict, "ms": elapsed, "status": status,
                     "html_kb": len(html) // 1024, "extracted": len(extracted),
                     "preview": " ".join(extracted.split())[:150]}

json.dump(results, open("bench_stealth_results.json", "w"), indent=2)
print(f"{'site':<12}{'verdict':<28}{'ms':>7}{'html_kb':>9}{'extracted':>10}")
print("-" * 66)
for name, r in results.items():
    print(f"{name:<12}{r['verdict']:<28}{r['ms']:>7}{r.get('html_kb','-'):>9}{r.get('extracted','-'):>10}")
    if r.get("preview"):
        print(f"             > {r['preview']}")
