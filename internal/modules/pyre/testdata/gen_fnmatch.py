"""Generate fnmatch.json: CPython's fnmatch.translate and fnmatchcase
results the Go port must reproduce.

    python3 internal/modules/pyre/testdata/gen_fnmatch.py internal/modules/pyre/testdata/fnmatch.json
"""

import fnmatch
import json
import sys

PATTERNS = [
    "", "*", "**", "?", "*.txt", "a*b*c", "*a*", "a?c", "[abc]", "[!abc]",
    "[a-c]x", "[!a-c]", "[]]", "[!]]", "[]-]", "[a-]", "[-a]", "[!-a]",
    "[z-a]", "[a-c-e]", "[a-a]", "[^a]", "[[]", "[\\]", "[a\\-z]", "[&&]",
    "[~~x]", "[||]", "[", "[!", "[!]", "[]", "a[", "*[a-z]*", "x*y?z*",
    "foo.bar", "f(o)o+", "a|b", "$x^", "*.{yml,yaml}", "web[0-9][0-9]",
    "db-*", "ünï*cødé", "*\\*", "a/b*", "*/*", ".*", "[.]*", "[!.]*",
    "x[b-a]y", "[%-0]", "[--0]", "[!--0]", "[a-b-c-d]", "[a--b]",
]

NAMES = [
    "", "a", "b", "abc", "a.txt", "x.txt", "txt", "aXbYc", "acb", "abbc",
    "]", "-", "!", "^", "[", "\\", "&", "~", "|", "x", "foo.bar", "fooxbar",
    "f(o)o+", "a|b", "$x^", "web01", "web1", "db-1", "ünïXcødé", "a*",
    "a/b/c", "a/bc", ".hidden", "visible", "xy", "x\ny", "\n", "%", "/",
    "0", ".", "y", "a\nb",
]

out = []
for p in PATTERNS:
    out.append({
        "pattern": p,
        "translate": fnmatch.translate(p),
        "matches": [n for n in NAMES if fnmatch.fnmatchcase(n, p)],
    })

with open(sys.argv[1], "w") as f:
    json.dump({"names": NAMES, "cases": out}, f, indent=1, ensure_ascii=False)
    f.write("\n")
