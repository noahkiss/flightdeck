#!/usr/bin/env python3
"""Word error rate of a transcript against a reference, for the hab Parakeet spike.

Usage: wer.py <reference> <hypothesis>
       wer.py --text <sherpa-onnx log>   (print the transcript from a CLI log)

Both texts are normalised the same way: camelCase and snake_case split into words,
"." "_" "/" "-" read as spaces, spoken symbol names ("dot", "underscore") dropped,
case and punctuation removed. So "link_test.go" and "link underscore test dot go" match.

Prints three tab-separated fields: WER, word errors / reference words, and identifiers
written exactly (identifiers are reference words with an inner capital, "_" or ".").
"""
import json
import re
import sys

SPOKEN_SYMBOLS = {"dot", "underscore", "slash", "dash", "hyphen"}


def words(text):
    text = re.sub(r"([a-z0-9])([A-Z])", r"\1 \2", text)
    text = re.sub(r"[._/\-]", " ", text.lower())
    text = re.sub(r"[^a-z0-9' ]", " ", text)
    return [w for w in text.split() if w not in SPOKEN_SYMBOLS]


def distance(ref, hyp):
    prev = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        cur = [i]
        for j, h in enumerate(hyp, 1):
            cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (r != h)))
        prev = cur
    return prev[-1]


def identifiers(text):
    out = []
    for tok in text.split():
        tok = tok.strip(",;:!?()\"'")
        tok = tok.rstrip(".")
        if re.search(r"[a-z][A-Z]|_|\w\.\w", tok):
            out.append(tok)
    return out


def log_text(path):
    # The CLI wraps its long JSON result line, so join the log before searching it.
    log = open(path, encoding="utf-8").read().replace("\n", "")
    m = re.search(r'"text": ("(?:[^"\\]|\\.)*")', log)
    return json.loads(m.group(1)) if m else ""


def main():
    if sys.argv[1] == "--text":
        print(log_text(sys.argv[2]))
        return
    ref_text, hyp_text = sys.argv[1], sys.argv[2]
    ref, hyp = words(ref_text), words(hyp_text)
    errors = distance(ref, hyp)
    ids = identifiers(ref_text)
    exact = sum(1 for i in ids if i in hyp_text)
    print(f"{errors / len(ref):.3f}\t{errors}/{len(ref)}\t{exact}/{len(ids)}")


if __name__ == "__main__":
    main()
