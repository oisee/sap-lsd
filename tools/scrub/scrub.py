#!/usr/bin/env python3
"""Scrub a DIAG capture of private identifiers, keeping every frame the SAME
LENGTH so the wire structure stays valid.

    scrub.py IN.jsonl OUT.jsonl [tokens.json]

The real identifiers live in tokens.json — which is gitignored on purpose: that
file IS the list of what you are hiding, so it must never be committed. Copy
tokens.example.json to tokens.json and fill in your own values (find them with a
hexdump; e.g. the ST_R3INFO codepage GUID follows the marker 10 06 11 00 20).

tokens.json shape (every replacement must be the same length as its target):
    {
      "hex":   { "<real 32B GUID hex>": "<dummy 32B hex>" },
      "ascii": { "<real host/ip/user>": "<dummy, same length>" },
      "regex": [ ["-15-[0-9A-Fa-f]{32}-", "-15-00000000000000000000000000000000-"] ]
    }
"""
import json, re, sys, os

def sl(a, b):
    if len(a) != len(b):
        sys.exit("length mismatch (must scrub same-length): %r -> %r" % (a, b))
    return (a, b)

def load_tokens(path):
    if not os.path.exists(path):
        sys.exit("no %s (copy tokens.example.json -> tokens.json and fill in your values)" % path)
    t = json.load(open(path))
    hexs  = [sl(bytes.fromhex(k), bytes.fromhex(v)) for k, v in t.get("hex", {}).items()]
    ascii = [sl(k.encode(), v.encode()) for k, v in t.get("ascii", {}).items()]
    regex = [(re.compile(p.encode()), r.encode()) for p, r in t.get("regex", [])]
    return hexs, ascii, regex

def scrub(b, hexs, ascii, regex):
    for a, c in hexs + ascii:
        b = b.replace(a, c)
    for pat, rep in regex:
        b = pat.sub(rep, b)
    return b

def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    src, dst = sys.argv[1], sys.argv[2]
    toks = sys.argv[3] if len(sys.argv) > 3 else os.path.join(os.path.dirname(__file__), "tokens.json")
    hexs, ascii, regex = load_tokens(toks)
    with open(src) as f, open(dst, "w") as o:
        for line in f:
            r = json.loads(line)
            r["hex"] = scrub(bytes.fromhex(r["hex"]), hexs, ascii, regex).hex()
            o.write(json.dumps(r) + "\n")
    print("scrubbed", src, "->", dst)

if __name__ == "__main__":
    main()
