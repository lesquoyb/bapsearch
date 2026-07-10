"""Benchmark: trafilatura vs Scrapling on identical real-page HTML.

Measures, per page and per extractor:
  - median wall time over N runs (warm, in-process — mirrors the long-lived
    trafilatura service used by bap-search)
  - output length and failure (empty/None/exception)
  - boilerplate contamination (distinct noise markers present in the output)
  - 5-gram containment overlap between the two outputs

Extractors:
  trafilatura      trafilatura.extract(html)  — exactly what the service does
  scrapling_text   Scrapling CLI "text" path: visible body text, noise tags +
                   hidden elements stripped (NOT article extraction)
  scrapling_md     Scrapling CLI "markdown" path (markdownify of the body)
"""
import json
import pathlib
import statistics
import time

import trafilatura
from scrapling.core.shell import Convertor
from scrapling.parser import Selector

RUNS = 5
NOISE_MARKERS = [
    "cookie", "newsletter", "subscribe", "s'abonner", "abonnez-vous",
    "sign in", "log in", "se connecter", "all rights reserved",
    "tous droits réservés", "privacy policy", "politique de confidentialité",
    "advertisement", "publicité", "©",
]


def run_trafilatura(html: str) -> str:
    return trafilatura.extract(html) or ""


def run_trafilatura_fast(html: str) -> str:
    # fast=True skips the readability/justext fallback cascade.
    return trafilatura.extract(html, fast=True) or ""


def run_scrapling_text(html: str) -> str:
    page = Selector(html)
    return "".join(Convertor._extract_content(page, extraction_type="text", main_content_only=True))


# Heuristic main-content selectors, most specific first. Mimics a lightweight
# readability on top of Scrapling's fast parser.
MAIN_SELECTORS = (
    "article", "main", "[role='main']", "#content", ".post-content", ".article-body",
)


def run_scrapling_main(html: str) -> str:
    page = Selector(html)
    target = None
    for sel in MAIN_SELECTORS:
        found = page.css(sel)
        if found:
            # Pick the candidate with the most text among the first matches.
            target = max(found[:3], key=lambda e: len(e.text or ""))
            break
    if target is None:
        target = page.css("body").first or page
    return "".join(Convertor._extract_content(target, extraction_type="text", main_content_only=False))


def run_scrapling_md(html: str) -> str:
    page = Selector(html)
    return "".join(Convertor._extract_content(page, extraction_type="markdown", main_content_only=True))


EXTRACTORS = {
    "trafilatura": run_trafilatura,
    "trafilatura_fast": run_trafilatura_fast,
    "scrapling_text": run_scrapling_text,
    "scrapling_main": run_scrapling_main,
    "scrapling_md": run_scrapling_md,
}


def five_grams(text: str) -> set:
    words = text.lower().split()
    return {" ".join(words[i:i + 5]) for i in range(len(words) - 4)}


def noise_score(text: str) -> int:
    low = text.lower()
    return sum(1 for marker in NOISE_MARKERS if marker in low)


def main():
    corpus = sorted(pathlib.Path("corpus").glob("*.html"))
    samples_dir = pathlib.Path("samples")
    samples_dir.mkdir(exist_ok=True)

    # Warm-up (first call pays lazy-loading costs in both libraries)
    warm = corpus[0].read_bytes().decode("utf-8", errors="replace")
    for fn in EXTRACTORS.values():
        try:
            fn(warm)
        except Exception:
            pass

    results = {}
    for path in corpus:
        name = path.stem
        html = path.read_bytes().decode("utf-8", errors="replace")
        page_result = {"html_kb": len(html) // 1024}
        outputs = {}
        for ex_name, fn in EXTRACTORS.items():
            times, output, error = [], "", ""
            for _ in range(RUNS):
                t0 = time.perf_counter()
                try:
                    output = fn(html)
                except Exception as exc:  # noqa: BLE001
                    error = f"{type(exc).__name__}: {exc}"
                    output = ""
                times.append(time.perf_counter() - t0)
            outputs[ex_name] = output
            page_result[ex_name] = {
                "median_ms": round(statistics.median(times) * 1000, 1),
                "chars": len(output),
                "empty": len(output.strip()) == 0,
                "error": error,
                "noise_markers": noise_score(output),
            }
            (samples_dir / f"{name}.{ex_name}.txt").write_text(output, encoding="utf-8")

        # Reference-based metrics against standard trafilatura output:
        # containment (5-gram coverage) and the char offset where the
        # reference content starts in each output. bap-search's answer prompt
        # only sees the FIRST ~3000 chars, so a large offset means the useful
        # content is pushed out of the LLM's window.
        ref = outputs["trafilatura"]
        ref_grams = five_grams(ref)
        probe = " ".join(ref.split())[:60].lower()
        for ex_name in EXTRACTORS:
            out_norm = " ".join(outputs[ex_name].split()).lower()
            grams = five_grams(outputs[ex_name])
            cell = page_result[ex_name]
            cell["ref_coverage"] = round(len(ref_grams & grams) / len(ref_grams), 3) if ref_grams else None
            cell["content_offset"] = out_norm.find(probe) if probe else None
        results[name] = page_result

    pathlib.Path("bench_results.json").write_text(json.dumps(results, indent=2))

    # ---- per-page tables ----
    for metric, label in (("median_ms", "median ms"), ("chars", "output chars"), ("content_offset", "offset of reference content (chars; -1 = probe not found)")):
        print(f"\n== {label} ==")
        print(f"{'page':<15}{'kb':>5}" + "".join(f"{ex:>18}" for ex in EXTRACTORS))
        for name, r in results.items():
            print(f"{name:<15}{r['html_kb']:>5}" + "".join(f"{str(r[ex].get(metric)):>18}" for ex in EXTRACTORS))

    print("\n== aggregates ==")
    for ex in EXTRACTORS:
        medians = [r[ex]["median_ms"] for r in results.values()]
        empties = sum(1 for r in results.values() if r[ex]["empty"])
        errors = sum(1 for r in results.values() if r[ex]["error"])
        noise = sum(r[ex]["noise_markers"] for r in results.values())
        covs = [r[ex]["ref_coverage"] for r in results.values() if r[ex]["ref_coverage"] is not None]
        print(f"{ex:<18} total {sum(medians):>6.0f} ms  median/page {statistics.median(medians):>7.1f} ms  empty {empties}/{len(results)}  exceptions {errors}  noise {noise:>3}  ref-coverage {statistics.median(covs):.2f}")


if __name__ == "__main__":
    main()
