"""Generates dump.json for TestDumpMatchesPyYAML: what ansible-core's
to_yaml / to_nice_yaml filters (PyYAML's SafeRepresenter over libyaml's
emitter) produce for each input and keyword arguments. Inputs are encoded
with their Python types; a container referenced twice carries an "id" that
"ref" entries point back at. Test-data tooling only; run it with the Python
ansible-core is installed in.

Usage: python3 dump_oracle.py > dump.json
"""
import json, random, sys
from ansible.plugins.filter.core import to_yaml, to_nice_yaml

STRINGS = [
    "", " ", "  ", "a", "abc", "hello world", "yes", "no", "Yes", "NO", "on", "off", "On", "y", "n", "true", "False",
    "~", "null", "Null", "NULL", "none", "None", "0", "1", "-1", "+1", "007", "0o17", "0x1F", "0b101", "1_000",
    "1.5", "1e5", "1e+5", "1.0e+5", ".5", "5.", ".inf", "-.inf", ".nan", ".NaN", "1:30", "190:20:30", "1:30.5",
    "2001-12-14", "2001-12-14t21:59:43.10-05:00", "2001-12-14 21:59:43.10 -5", "2002-1-1 1:2:3", "2020-01-01T00:00:00Z",
    "<<", "=", "-", "--", "---", "--- x", "...", "....", "-a", "- a", "?", "? a", "?a", ":", ":a", ": a", "a:", "a: b",
    "a:b", "a :b", "http://x.y/z?q=1#f", "a #b", "a#b", "#a", ",a", "a,b", "[a]", "a[b]", "{a}", "a{b}", "&a", "*a",
    "!a", "|a", ">a", "'a", "\"a", "%a", "@a", "`a", "a'b", "a\"b", "a\\b", "it's", "say \"hi\"",
    " lead", "trail ", " both ", "\ttab", "tab\t", "a\tb", "a\nb", "a\n", "\na", "a\n\n", "a\n\nb", "a\n \nb",
    "a \nb", "a\n b", "line1\nline2\nline3\n", "\n", "\n\n", "a\r\nb", "a\rb", "a\x85b", "a\u2028b", "a\u2029b",
    "\x00", "\x07", "\x1b[0m", "\x7f", "caf\u00e9", "\u00a0nbsp", "a\u00a0b", "\u00e9t\u00e9", "\u65e5\u672c\u8a9e",
    "emoji \U0001F600", "\ufeffbom", "\ufffd", "\ufffe", "\ud7ff", "\ue000", "a\u0085",
    "x" * 79, "x" * 81, "x" * 200, ("word " * 30).strip(), ("word " * 30), "a " * 50, "  ".join(["ab"] * 40),
    ("long line with spaces that goes on and on " * 4) + "\nsecond line\n", "a  b", "a   b" * 30,
    ("x" * 70) + " " + ("y" * 20), ("x" * 78) + " a b c",
    "key: value\nother: thing\n", "#!/bin/bash\necho hi\n", "- item\n- item2", "{{ templ }}", "{% raw %}",
    "C:\\path\\to", "user@host", "100%", "$HOME", "a;b", "(x)", "a|b", "a>b", "a=b", "é: x", "\"\"", "''",
]

KEYS = ["a", "b", "key", "yes", "1", "", " ", "a b", "a: b", "-", "?", "é", "k\nl", "x" * 130, "x" * 127,
        "null", "~", "#", "[x]", "Key", "Z", "_", "10", "2"]


def enc(v, counts, ids, seen):
    if v is None:
        return {"t": "none"}
    if isinstance(v, bool):
        return {"t": "bool", "v": v}
    if isinstance(v, int):
        return {"t": "int", "v": str(v)}
    if isinstance(v, float):
        return {"t": "float", "v": repr(v)}
    if isinstance(v, str):
        return {"t": "str", "v": v}
    i = id(v)
    if i in seen:
        return {"t": "ref", "id": ids[i]}
    out = {}
    if counts.get(i, 0) > 1:
        ids[i] = len(ids) + 1
        out["id"] = ids[i]
    seen.add(i)
    if isinstance(v, list):
        out["t"] = "list"
        out["v"] = [enc(x, counts, ids, seen) for x in v]
    else:
        out["t"] = "dict"
        out["v"] = [[k, enc(x, counts, ids, seen)] for k, x in v.items()]
    return out


def count(v, counts, stack=()):
    if isinstance(v, (list, dict)):
        i = id(v)
        counts[i] = counts.get(i, 0) + 1
        if counts[i] > 1:
            return
        for x in (v if isinstance(v, list) else v.values()):
            count(x, counts)


cases = []


def add(name, value, filt="to_yaml", **kw):
    fn = to_yaml if filt == "to_yaml" else to_nice_yaml
    try:
        out = fn(value, **kw)
    except Exception as e:
        print("skip", name, e, file=sys.stderr)
        return
    counts = {}
    count(value, counts)
    cases.append({"name": name, "filter": filt, "kwargs": kw, "in": enc(value, counts, {}, set()), "out": out})


for i, s in enumerate(STRINGS):
    add("str%d" % i, s)
    add("strnice%d" % i, s, "to_nice_yaml")
    add("strlist%d" % i, [s])
    add("strmap%d" % i, {"k": s})
    add("strkey%d" % i, {s: 1})
    add("strnest%d" % i, {"k": [s, {"n": s}]}, "to_nice_yaml")
    add("strdq%d" % i, s, default_style='"')
    add("strsq%d" % i, {"k": s}, default_style="'")
    add("strlit%d" % i, {"k": s}, default_style="|")
    add("strfold%d" % i, [s], default_style=">")
    add("strw20_%d" % i, {"k": s}, width=20)
    add("strflow%d" % i, {"k": [s]}, default_flow_style=True)

for k in KEYS:
    add("key_" + k, {k: "v", "z": [1]}, "to_yaml")
    add("nkey_" + k, {k: {"x": 1}}, "to_nice_yaml")

SCALARS = [None, True, False, 0, 1, -5, 123456789012, 1.0, -0.0, 0.5, 1e16, 1e15, 123456789.123, 1e-5, 1e-4,
           2.5e-10, 1.7976931348623157e308, float("inf"), float("-inf"), float("nan"), 3.14159, 100.0, 1e100]
for i, s in enumerate(SCALARS):
    add("scalar%d" % i, s)
    add("scalarl%d" % i, {"v": s, "l": [s, s]}, "to_nice_yaml")
    add("scalardq%d" % i, {"v": s}, default_style='"')
    add("scalarcanon%d" % i, [s], canonical=True)

SHAPES = {
    "empty_list": [], "empty_dict": {}, "nested_empty": {"a": [], "b": {}, "c": [[]], "d": [{}]},
    "list_of_lists": [[1, 2], [3, [4, 5]], []], "list_of_dicts": [{"a": 1, "b": 2}, {"c": [1, 2]}],
    "deep": {"a": {"b": {"c": {"d": {"e": [1, {"f": "g"}]}}}}},
    "mixed": {"name": "web", "ports": [80, 443], "env": {"A": "1", "B": "two"}, "tags": [], "n": None},
    "unsorted": {"z": 1, "a": 2, "m": {"y": 1, "b": 2}}, "upper": {"B": 1, "a": 2, "_": 3, "1": 4, "é": 5},
    "longflow": list(range(40)), "longflowstr": ["item%d" % i for i in range(30)],
    "longflowmap": {"key%d" % i: i for i in range(20)}, "nestedflow": {"a": {"k%d" % i: "value%d" % i for i in range(15)}},
    "seqseq": [[[1]]], "mapinseq": [{"a": [1, 2], "b": {"c": 3}}], "dictlist_in_list": [[{"a": 1}]],
    "multiline_in_list": ["a\nb", "c"], "multiline_in_map": {"a": "line1\nline2\n", "b": "x"},
}
for name, v in SHAPES.items():
    for filt in ("to_yaml", "to_nice_yaml"):
        add(name, v, filt)
    for ind in (1, 2, 3, 5, 9, 10):
        add("%s_i%d" % (name, ind), v, "to_nice_yaml", indent=ind)
        add("%s_yi%d" % (name, ind), v, indent=ind)
    for w in (-1, 0, 10, 40, 100):
        add("%s_w%d" % (name, w), v, width=w)
        add("%s_nw%d" % (name, w), v, "to_nice_yaml", width=w)
    add(name + "_nosort", v, sort_keys=False)
    add(name + "_nnosort", v, "to_nice_yaml", sort_keys=False)
    add(name + "_start", v, explicit_start=True)
    add(name + "_end", v, explicit_end=True)
    add(name + "_startend", v, "to_nice_yaml", explicit_start=True, explicit_end=True)
    add(name + "_flow", v, default_flow_style=True)
    add(name + "_block", v, default_flow_style=False)
    add(name + "_canon", v, canonical=True)
    add(name + "_dq", v, default_style='"')
    add(name + "_crlf", v, line_break="\r\n")
    add(name + "_cr", v, line_break="\r")

for s in ["abc", "a\nb", "", "x" * 100]:
    for kw in ({"explicit_start": True}, {"explicit_end": True}, {"explicit_start": True, "explicit_end": True}):
        add("rootstr_" + str(sorted(kw)), s, **kw)
for s in ["a\n\n", "\n", "a\n", "a", " a\n", "\na"]:
    add("litroot", s, default_style="|")
    add("foldroot", s, default_style=">")
    add("litrootend", s, default_style="|", explicit_end=True)
    add("litmap", {"k": s, "j": 1}, default_style="|")

# Aliases.
x = [1, 2]
y = {"k": "v"}
e = []
ed = {}
add("alias1", {"a": x, "b": x, "c": y, "d": y})
add("alias2", [x, x])
add("alias3", {"a": {"inner": x}, "b": x})
add("alias4", [y, y], "to_nice_yaml")
add("alias5", {"a": e, "b": e, "c": ed, "d": ed})
add("alias6", [x, [x, y], {"q": y}], "to_nice_yaml", indent=2)
rec = [1]
rec.append(rec)
add("recursive_list", rec)
recd = {"a": 1}
recd["self"] = recd
add("recursive_dict", recd)
add("recursive_dict_nice", recd, "to_nice_yaml")

# Random fuzz.
rnd = random.Random(42)
ALPHA = list("abcxyz ABZ019:#-?,[]{}'\"\\\n\t .&*!|>%@`é日\u00a0\u2028") + ["\U0001F600", "\x85", "\r"]


def rstr():
    n = rnd.choice([0, 1, 2, 3, 5, 8, 20, 50, 90])
    return "".join(rnd.choice(ALPHA) for _ in range(n))


def rval(d):
    r = rnd.random()
    if d > 3 or r < 0.5:
        return rnd.choice([None, True, 1, -3, 2.5, rstr(), rstr(), rstr(), rnd.choice(STRINGS)])
    if r < 0.75:
        return [rval(d + 1) for _ in range(rnd.randint(0, 5))]
    return {rstr() if rnd.random() < 0.5 else rnd.choice(KEYS): rval(d + 1) for _ in range(rnd.randint(0, 5))}


for i in range(150):
    v = rval(0)
    filt = rnd.choice(["to_yaml", "to_nice_yaml"])
    kw = {}
    if rnd.random() < 0.3:
        kw["width"] = rnd.choice([10, 30, 50, 120])
    if rnd.random() < 0.2:
        kw["indent"] = rnd.randint(2, 9)
    add("fuzz%d" % i, v, filt, **kw)

sys.stdout.write("[\n" + ",\n".join(json.dumps(c, ensure_ascii=True) for c in cases) + "\n]\n")
