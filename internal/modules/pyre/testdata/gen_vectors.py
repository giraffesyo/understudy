"""Generate vectors.json: CPython's re results the Go port must reproduce.

    python3 internal/modules/pyre/testdata/gen_vectors.py internal/modules/pyre/testdata/vectors.json

Spans are converted to UTF-8 byte offsets (the Go API's indices).
"""

import json
import re
import sys
import warnings

warnings.simplefilter("ignore")

FLAGS = {"i": re.I, "m": re.M, "s": re.S, "x": re.X, "a": re.A, "u": re.U}


def boff(s, i):
    return len(s[:i].encode("utf-8", "surrogatepass"))


def spans(s, m):
    if m is None:
        return None
    out = []
    for g in range(m.re.groups + 1):
        a, b = m.span(g)
        if a < 0:
            out += [-1, -1]
        else:
            out += [boff(s, a), boff(s, b)]
    return out


def exc(e):
    return {"exc": type(e).__name__, "msg": str(e)}


def flagval(fs):
    v = 0
    for c in fs:
        v |= FLAGS[c]
    return int(v)


# (pattern, flags, [subjects], [templates])
MATCH = [
    (r"abc", "", ["abc", "xabcx", "ab", ""], [r"-", r"[\g<0>]"]),
    (r"a|b|c", "", ["xbx", "cab"], [r"<\g<0>>"]),
    (r"x*", "", ["abxd", "", "xxx"], [r"-"]),
    (r"\s*$", "", ["ab  ", "ab\n", "a\nb \n"], [r"-"]),
    (r"\b", "", ["ab cd", "", " "], [r"-"]),
    (r"\B", "", ["ab cd", "", " ", "a"], [r"-"]),
    (r"(a|ab)(c|bcd)(d*)", "", ["abcd"], [r"\1-\2-\3"]),
    (r"(a*)*", "", ["b", "aab"], [r"<\1>"]),
    (r"(a*)+", "", ["b", "aab"], [r"<\1>"]),
    (r"(a|b)*", "", ["ab", "abba", ""], [r"<\1>"]),
    (r"(?:(a)|b)*", "", ["ab", "ba"], [r"<\1>"]),
    (r"((a)|b)+", "", ["ab", "ba"], [r"<\1\2>"]),
    (r"(a)|(b)", "", ["b", "a", "c"], [r"[\1|\2]"]),
    (r"(?P<first>\w+) (?P<last>\w+)", "", ["Jane Doe", "x y z"], [r"\g<last>, \g<first>", r"\2 \1", r"\g<2>\g<1>"]),
    (r"(?P<a>x)(?P=a)", "", ["xx", "xy", "axxa"], [r"\g<a>"]),
    (r"(a)\1", "", ["aa", "ab"], [r"\1"]),
    (r"(a)(?(1)b|c)", "", ["ab", "ac"], []),
    (r"(a)?(?(1)b|c)", "", ["ab", "c", "ac", "b"], []),
    (r"(?P<q>')?\w+(?(q)')", "", ["'abc'", "abc", "'abc"], []),
    (r"foo(?=bar)", "", ["foobar", "foobaz"], [r"X"]),
    (r"foo(?!bar)", "", ["foobar", "foobaz"], [r"X"]),
    (r"(?<=abc)def", "", ["abcdef", "xbcdef"], [r"X"]),
    (r"(?<!abc)def", "", ["abcdef", "xbcdef", "def"], [r"X"]),
    (r"(?<=\d{3})x", "", ["123x", "12x"], []),
    (r"(?<=(a))b", "", ["ab"], [r"\1"]),
    (r"(?=(a))a", "", ["a"], [r"\1"]),
    (r"(?>a+)b", "", ["aaab", "aaa"], []),
    (r"(?>a|ab)c", "", ["abc", "ac"], []),
    (r"a++b", "", ["aaab"], []),
    (r"a++a", "", ["aaaa"], []),
    (r"(?:ab)++a", "", ["ababa", "abab"], []),
    (r"(?:a|ab)++c", "", ["abc", "aac"], []),
    (r"a*+", "", ["aaa", "b"], [r"-"]),
    (r"a?+b", "", ["ab", "b"], []),
    (r"a{2,3}+", "", ["aaaa"], []),
    (r"a*?", "", ["aaa"], [r"-"]),
    (r"a+?", "", ["aaa"], [r"-"]),
    (r"a{2,3}?", "", ["aaaa"], [r"-"]),
    (r"a{,2}", "", ["aaaa"], [r"-"]),
    (r"a{2,}", "", ["a", "aaaa"], [r"-"]),
    (r"a{2}", "", ["aaaaa"], [r"-"]),
    (r"a{}", "", ["a{}"], []),
    (r"a{1", "", ["a{1"], []),
    (r"a{1,2", "", ["a{1,2"], []),
    (r"a{,}", "", ["a{,}", "aaa"], []),
    (r"x{2}y", "", ["xxy"], []),
    (r"(?:a|b)*?c", "", ["abc"], []),
    (r"(a|b)*?c", "", ["abac"], [r"\1"]),
    (r"(ab)*?(ab)", "", ["ababab"], [r"\1:\2"]),
    (r"(a+?)(a*)", "", ["aaa"], [r"\1:\2"]),
    (r"(?:x*)*", "", ["xx"], [r"-"]),
    (r"(?:x*)*?y", "", ["xxy"], []),
    (r"(?:()|a)*", "", ["aa"], []),
    (r"(?:a|())*", "", ["aa"], []),
    (r"(?:a?)*b", "", ["aab"], []),
    (r"(?:a??)+?b", "", ["aab"], []),
    (r"(?:(a)|b)*?c", "", ["abc"], []),
    (r"^", "", ["", "a\nb"], [r"-"]),
    (r"^", "m", ["", "a\nb\n"], [r"-"]),
    (r"$", "", ["", "a\nb\n", "a\nb"], [r"-"]),
    (r"$", "m", ["", "a\nb\n", "a\nb"], [r"-"]),
    (r"^\w+$", "m", ["ab\ncd\n"], [r"<\g<0>>"]),
    (r"\Aa", "m", ["a\na"], [r"-"]),
    (r"a\Z", "m", ["a\na", "a\na\n"], [r"-"]),
    (r"a\z", "", ["a\na", "a\na\n"], [r"-"]),
    (r".", "", ["a\nb"], [r"-"]),
    (r".", "s", ["a\nb"], [r"-"]),
    (r".+", "", ["ab\ncd"], [r"-"]),
    (r"(?s).+", "", ["ab\ncd"], [r"-"]),
    (r"(?s:.)\n.", "", ["a\nb\n\n"], [r"-"]),
    (r"(?-s:.)", "s", ["a\nb"], [r"-"]),
    (r"a(?i)b", "", ["ab"], []),
    (r"(?i)ab", "", ["AB", "aB"], [r"-"]),
    (r"(?i:a)b", "", ["Ab", "AB"], [r"-"]),
    (r"(?-i:a)b", "i", ["Ab", "aB"], [r"-"]),
    (r"(?x) a b  c # comment", "", ["abc"], [r"-"]),
    (r"a b # c", "x", ["ab"], [r"-"]),
    (r"(?x)[ ]a", "", [" a"], [r"-"]),
    (r"a\ b", "x", ["a b"], [r"-"]),
    (r"(?x: a b ) c", "", ["ab c"], [r"-"]),
    (r"a(?#comment)b", "", ["ab"], [r"-"]),
    (r"[abc]", "", ["xbx"], [r"-"]),
    (r"[^abc]", "", ["abxc\n"], [r"-"]),
    (r"[a-c]+", "", ["xabcdx"], [r"-"]),
    (r"[]a]", "", ["]a"], [r"-"]),
    (r"[^]a]", "", ["]ab"], [r"-"]),
    (r"[a-]", "", ["-a"], [r"-"]),
    (r"[-a]", "", ["-a"], [r"-"]),
    (r"[\w-]", "", ["a-b!"], [r"-"]),
    (r"[\d\s]", "", ["a1 b"], [r"-"]),
    (r"[\D]", "", ["a1 b"], [r"-"]),
    (r"[\S\s]", "", ["a1\nb"], [r"-"]),
    (r"[^\S\s]", "", ["a1\nb"], [r"-"]),
    (r"[\]]", "", ["a]"], [r"-"]),
    (r"[\\]", "", ["a\\"], [r"-"]),
    (r"[\x41-\x43]", "", ["ABCD"], [r"-"]),
    (r"[\u00e0-\u00e5]", "", ["àáâé"], [r"-"]),
    (r"[\101]", "", ["A"], [r"-"]),
    (r"\101\x41\u0041\U00000041\N{LATIN CAPITAL LETTER A}", "", ["AAAAA"], [r"-"]),
    (r"\N{EM DASH}\N{em dash}", "", ["——"], [r"-"]),
    (r"\0\07\177", "", ["\0\x07\x7f"], [r"-"]),
    (r"\a\f\n\r\t\v\\", "", ["\a\f\n\r\t\v\\"], [r"-"]),
    (r"\d+", "", ["a123b٣٤", "x"], [r"-"]),
    (r"\d+", "a", ["a123b٣٤"], [r"-"]),
    (r"\w+", "", ["héllo wörld_1 日本"], [r"-"]),
    (r"\w+", "a", ["héllo wörld_1"], [r"-"]),
    (r"\s+", "", ["a \t\n\x0b\x0c\r\x1c\x85\xa0\u2003b"], [r"-"]),
    (r"\s+", "a", ["a \t\n\x0b\x0c\r\x1c\x85\xa0b"], [r"-"]),
    (r"\bé", "", ["é aé"], [r"-"]),
    (r"\bé", "a", ["é aé"], [r"-"]),
    (r"(?i)straße", "", ["STRASSE", "STRAßE", "straße"], [r"-"]),
    (r"(?i)ſ", "", ["s", "S", "ſ"], [r"-"]),
    (r"(?i)s", "", ["s", "S", "ſ"], [r"-"]),
    (r"(?i)[s]", "", ["s", "S", "ſ"], [r"-"]),
    (r"(?i)[r-t]", "", ["s", "S", "ſ"], [r"-"]),
    (r"(?i)k", "", ["k", "K", "\u212a"], [r"-"]),
    (r"(?i)[^k]", "", ["k", "K", "\u212a", "x"], [r"-"]),
    (r"(?i)[a-z]+", "", ["HeLLo\u212a\u0131\u0130"], [r"-"]),
    (r"(?ai)[a-z]+", "", ["HeLLo\u212a\u0131"], [r"-"]),
    (r"(?ai)k", "", ["k", "K", "\u212a"], [r"-"]),
    (r"(?i)i", "", ["i", "I", "\u0131", "\u0130"], [r"-"]),
    (r"(?i)\u0130", "", ["i", "I", "\u0131", "\u0130"], [r"-"]),
    (r"(?i)µ", "", ["µ", "μ", "Μ"], [r"-"]),
    (r"(?i)Σ", "", ["σ", "ς", "Σ"], [r"-"]),
    (r"(?i)(a)\1", "", ["aA", "Aa"], [r"-"]),
    (r"(?i)(s)\1", "", ["sſ", "sS"], [r"-"]),
    (r"(?i)[\U00010400-\U00010410]", "", ["\U00010428", "\U00010400"], [r"-"]),
    (r"(?i)\U00010400", "", ["\U00010428", "\U00010400"], [r"-"]),
    (r"(?i)[\u02bc-\U00010000]", "", ["\u0149", "a"], [r"-"]),
    (r"(?i)[^a-z]", "", ["aZ1"], [r"-"]),
    (r"(?u)\w", "", ["é"], [r"-"]),
    (r"(?a:\w)\w", "", ["éé", "aé"], [r"-"]),
    (r"日本", "", ["こんにちは日本語"], [r"[\g<0>]"]),
    (r"(.)(.)", "", ["日本語abc"], [r"\2\1"]),
    (r"", "", ["", "abc", "日本"], [r"-"]),
    (r"|a", "", ["aba"], [r"-"]),
    (r"a|", "", ["aba"], [r"-"]),
    (r"(?:)", "", ["ab"], [r"-"]),
    (r"(?!)", "", ["ab"], [r"-"]),
    (r"(?=)", "", ["ab"], [r"-"]),
    (r"a(?=b)|ab", "", ["ab"], [r"-"]),
    (r"(a)|b", "", ["ab"], [r"[\1]"]),
    (r"(?P<n>a)|b", "", ["ab"], [r"[\g<n>]"]),
    (r"a|ab", "", ["ab"], [r"-"]),
    (r"(?:a|ab)(?:c|bcd)", "", ["abcd"], [r"-"]),
    (r"(a)(b)?", "", ["a", "ab"], [r"\2\1"]),
    (r"(\d+)-(\d+)", "", ["10-20 30-40"], [r"\2-\1", r"\g<2>0", r"\10", r"\1\0"]),
    (r"(a)(b)(c)(d)(e)(f)(g)(h)(i)(j)(k)", "", ["abcdefghijk"], [r"\11\10", r"\g<11>"]),
    (r"(a)(b)(c)(d)(e)(f)(g)(h)(i)(j)(k)\11", "", ["abcdefghijkk"], []),
    (r"(a)\1\01", "", ["aa\x01"], []),
    (r"x", "", ["x"], [r"\n\t\\", r"\-\.", r"a\0b", r"\012", r"\x"]),
    (r"(?:(?P<a>a)|(?P<b>b))+", "", ["abab"], [r"\g<a>\g<b>"]),
    (r"(a)(?:x|(b))*", "", ["axbx"], [r"\1\2"]),
    (r"\$\{(\w+)\}", "", ["${a} ${bb}"], [r"<\1>"]),
    (r"(?P<k>\w+)=(?P<v>[^;]*);?", "", ["a=1;b=2;c="], [r"\g<v>"]),
    (r"^(\s*)#?\s*(PermitRootLogin)\s", "m", ["# PermitRootLogin yes\nPermitRootLogin no\n"], [r"\1\2 "]),
    (r"(?<=^)a", "m", ["a\na"], [r"-"]),
    (r"(?<!^)a", "m", ["aa\naa"], [r"-"]),
    (r"(?<=a|b)c", "", ["acbc"], [r"-"]),
    (r"(?<=(?:ab|cd))x", "", ["abxcdx"], [r"-"]),
    (r"(?<=\b)x", "", ["x ax"], [r"-"]),
    (r"(\w)(?=(\w))", "", ["abc"], [r"\1\2"]),
    (r"(?=(\w+))", "", ["abc"], [r"<\1>"]),
    (r"(?:a(?=b))+", "", ["ab"], [r"-"]),
    (r"\w+(?<!ing)\b", "", ["sing singing kings"], [r"-"]),
    (r"(\d)(?!\d)", "", ["123 45"], [r"<\1>"]),
    (r"(?<=\$)\d+(?:\.\d\d)?", "", ["cost $12.50 or $3"], [r"-"]),
    (r"(\w+)\s+\1", "", ["the the cat cat"], [r"\1"]),
    (r"(?P<w>\w+)\s+(?P=w)", "", ["the the cat"], [r"\g<w>"]),
    (r"(a)(?(1)b)", "", ["ab", "a"], []),
    (r"(?(1)a|b)(x)?", "", ["b", "bx"], []),
    (r"(a)?(?(1)|x)", "", ["a", "x", "b"], []),
    (r"(?:(a)|b)(?(1)c|d)", "", ["ac", "bd", "ad", "bc"], []),
    (r"(?:x(a)?)*(?(1)y|z)", "", ["xaxy", "xxz", "xax z"], []),
    (r"a.c", "", ["a\nc", "abc"], [r"-"]),
    (r"[.]", "", ["a.c"], [r"-"]),
    (r"\.", "", ["a.c"], [r"-"]),
    (r"a\+b\?\*\(\)\[\]\{\}\|\^\$", "", ["a+b?*()[]{}|^$"], [r"-"]),
    (r"\#\ \&\~\-", "", ["# &~-"], [r"-"]),
    (r"(?m)^\s*$\n?", "", ["a\n\n  \nb\n"], [r""]),
    (r"\n", "", ["a\r\nb\n"], [r"\\n"]),
    (r"(a)(?P<x>b)(c)", "", ["abc"], [r"\3\g<x>\1"]),
    (r"(?:a|b|c)", "", ["abc"], [r"-"]),
    (r"(?:ab|ac)", "", ["abac"], [r"-"]),
    (r"(?:ab|a)(?:bc|c)", "", ["abc"], [r"-"]),
    (r"x(?:ab|a)*y", "", ["xababay"], [r"-"]),
    (r"(x+x+)+y", "", ["xxxxxxxxxxxxxxxxy", "xxxxxxxxxxxx"], []),
    (r"(\w+\s?)*$", "", ["an apple a day"], []),
    (r"(?i)hello (?-i:World)", "", ["HELLO World", "hello world"], []),
    (r"(?ims)^a.b$", "", ["A\nB\nc"], [r"-"]),
    (r"[\s\S]", "i", ["aB"], [r"-"]),
    (r"(?i)[\W]", "", ["aB_ !"], [r"-"]),
    (r"(?i)[^\d]", "", ["a1"], [r"-"]),
    (r"[^\W\d]", "", ["a1_é"], [r"-"]),
    (r"\W+", "", ["a, b!? c"], [r"-"]),
    (r"\D+", "", ["a12b"], [r"-"]),
    (r"\S+", "", ["a b\tc"], [r"-"]),
    (r"(ab)+?c", "", ["ababc"], [r"\1"]),
    (r"(a?)+", "", ["aa"], [r"<\1>"]),
    (r"(a?)+?", "", ["aa"], [r"<\1>"]),
    (r"(a|)+", "", ["aa"], [r"<\1>"]),
    (r"(|a)+", "", ["aa"], [r"<\1>"]),
    (r"(|a)*b", "", ["aab"], [r"<\1>"]),
    (r"(a*)*b", "", ["aab"], [r"<\1>"]),
    (r"(a*?)*b", "", ["aab"], [r"<\1>"]),
    (r"(a*)+?b", "", ["aab"], [r"<\1>"]),
    (r"(a{0,2}){2,3}", "", ["aaaaa", "a"], [r"<\1>"]),
    (r"(a{0,2}){2,3}?", "", ["aaaaa", "a"], [r"<\1>"]),
    (r"(?:a{0,2}){2,3}+", "", ["aaaaa"], []),
    (r"((a)|(b))*", "", ["abba"], [r"\1\2\3"]),
    (r"(?:(a)|(b)|c)*", "", ["abc", "acb"], [r"[\1\2]"]),
    (r"(a)*", "", ["aaa"], [r"<\1>"]),
    (r"(?:(a)b)*", "", ["ababa"], [r"<\1>"]),
    (r"(?:(a)|(a)b)*c", "", ["abac"], [r"[\1|\2]"]),
    (r"((?:a|b)*)c", "", ["abc"], [r"\1"]),
    (r"([^,]*),?", "", ["a,b,,c"], [r"<\1>"]),
    (r",", "", ["a,b,,c", ","], [r";"]),
    (r"(,)", "", ["a,b,,c"], [r";"]),
    (r"\W*", "", ["...words, words..."], [r"-"]),
    (r"(\W*)", "", ["...words..."], [r"-"]),
    (r"[\uff00-\uffff]", "", ["ａｂｃ"], [r"-"]),
    (r"[\U0001F600-\U0001F64F]+", "", ["hi 😀😃 there"], [r"-"]),
    (r".", "", ["😀a"], [r"-"]),
]

ERRORS = [
    "*", "a**", "a*{2}", "+", "?", "{1}", "(?", "(?P", "(?P<", "(?P<>a)", "(?P<1a>x)", "(?P<a>x)(?P<a>y)",
    "(?P=a)", "(?P<a>(?P=a))", "(", ")", "a)", "(a", "((a)", "[a", "[a-", "[", "[]", "[^]", "[b-a]",
    "[\\d-z]", "[a-\\d]", "\\", "a\\", "\\q", "[\\q]", "\\1", "(a)\\2", "(a\\1)", "\\x", "\\x4", "\\u12",
    "\\U1234567", "\\U00110000", "\\N", "\\N{", "\\N{}", "\\N{NO SUCH NAME}", "\\400", "[\\400]",
    "\\8", "[\\8]", "(?<=a+)b", "(?<=a|bc)d", "(?<=a*)b", "(?<!x{1,2})y", "(?<=(?P<a>x)(?P=a))",
    "(?<x)", "(?<", "(?Px)", "(?z)", "(?i", "(?i-", "(?i-:a)", "(?-:a)", "(?--i:a)", "(?ii-i:a)",
    "(?L)a", "(?au)a", "(?a-a:x)", "(?-u:x)", "a(?i)b", "(?i)(?m)a", "a|(?i)b", "(?x)(?i)a",
    "(?#abc", "(?(1)a|b)", "(?(1a)x)", "(?(0)a)", "(?(a)x)", "(?(1)a|b|c)", "(a)(?(1)a|b|c)",
    "(?(", "(?()", "(?(1", "x{2,1}", "x{4294967295}", "x{99999999999999999999}", "(?P>a)", "(?R)",
    "a\nb\n(", "\n*", "(?i:a", "(?i:", "(?<=x(?=y)z)", "(?<=\\1)(a)", "(a)(?<=\\1)", "(?<=(a)\\1)",
    "(?<=(a)(?(1)b|c))", "(a)(?<=(?(1)b|cd))", "(?<=(a)(?(2)b|c))(b)", "\\g<1>", "[a-\\w]",
    "(?x)a{1, 2}", "(?P<a-b>x)", "(?P<é>x)(?P=é)", "(?P<a>x)(?P=b)", "(?:", "(?:a|", "a{,4294967295}",
    "(?<=a{4294967296})b", "(?<=a{2147483648}b{2147483648})c", "(?<=x{3}|y{3})z", "(?<=(?:ab|cd))x",
    "(?-x)", "(?i-i:a)", "(?x-x:a)", "\\b*", "^*", "$+", "\\A?", "(?=a)*", "(?!a){2}", "a{1}{2}",
    "a*??", "a+?+", "a{1,2}?+", "\\Z*", "(?:)*", "()*",
]

TEMPLATE_ERRORS = [
    (r"(a)(?P<n>b)", [r"\g", r"\g<", r"\g<>", r"\g<x", r"\g<1a>", r"\g<-1>", r"\g<3>", r"\g<nope>", r"\3",
                      r"\9", r"\400", r"\q", "\\", r"\g<a b>", r"\g<99999999999999999999>", r"\00", r"\08",
                      r"\777", r"\1a", r"\g<0>\g<01>", r"\g<n>\g<2>", r"\w", r"\d"]),
    (r"x", [r"\1", r"\g<1>", r"\0", r"\g<0>", r"\\", r"\é"]),
]

SPLIT = [
    (r",", "a,b,,c", [0, 1, 2]),
    (r"(,)", "a,b,,c", [0, 2]),
    (r"(,)|(;)", "a,b;c", [0]),
    (r"x*", "axbc", [0]),
    (r"\b", "a bc", [0]),
    (r"", "abc", [0, 2]),
    (r"\W+", "Words, words, words.", [0, 1]),
    (r"(\W+)", "Words, words, words.", [0]),
    (r"[a-f]+", "0a3B9", [0]),
    (r"(?i)[a-f]+", "0a3B9", [0]),
]

ESCAPE = ["abc", "a.b*c", "()[]{}?*+-|^$\\.&~# \t\n\r\v\f", "日本語", "a_b-c", "!\"%'/:;<=>@`,", ""]


def compile_case(pat, fl):
    try:
        p = re.compile(pat, fl)
    except Exception as e:  # noqa: BLE001
        return exc(e)
    return {"groups": p.groups, "groupindex": dict(p.groupindex), "flags": int(p.flags)}


ATOMS = ["a", "b", "c", ".", "[ab]", "[^a]", "\\w", "\\W", "(?i:A)", "\\n", "[a-c]", "a", "k", "[k-s]", "\u00e9", "[^\\W\\d]", "\\d", "\\s"]
ANCHORS = ["\\b", "\\B", "^", "$", "\\A", "\\Z", ""]
QUANTS = ["", "", "", "*", "+", "?", "*?", "+?", "??", "{2}", "{1,2}", "{,2}", "{2,}?", "*+", "++", "?+", "{0,1}+"]


def gen_pattern(rng, depth, groups):
    n = rng.randint(1, 4)
    out = []
    for _ in range(n):
        r = rng.random()
        if depth < 3 and r < 0.35:
            kind = rng.choice(["cap", "cap", "nc", "alt", "look", "atomic", "named", "cond"])
            inner = gen_pattern(rng, depth + 1, groups)
            if kind == "cap":
                groups.append(None)
                out.append("(" + inner + ")")
            elif kind == "named":
                groups.append("g%d" % len(groups))
                out.append("(?P<%s>%s)" % (groups[-1], inner))
            elif kind == "nc":
                out.append("(?:" + inner + ")")
            elif kind == "alt":
                out.append("(?:" + inner + "|" + gen_pattern(rng, depth + 1, groups) + ")")
            elif kind == "look":
                out.append(rng.choice(["(?=", "(?!", "(?<=", "(?<!"]) + inner + ")")
            elif kind == "atomic":
                out.append("(?>" + inner + ")")
            else:
                if groups:
                    g = rng.randint(1, len(groups))
                    out.append("(?(%d)%s|%s)" % (g, inner, gen_pattern(rng, depth + 1, groups)))
            if out:
                out[-1] += rng.choice(QUANTS)
        elif r < 0.42 and groups:
            out.append("\\%d" % rng.randint(1, len(groups)))
        elif r < 0.5:
            out.append(rng.choice(ANCHORS))
        else:
            out.append(rng.choice(ATOMS) + rng.choice(QUANTS))
    return "".join(out)


def fuzz():
    import random
    rng = random.Random(20261001)
    cases = []
    while len(cases) < 8000:
        pat = gen_pattern(rng, 0, [])
        fl = rng.choice([0, 0, re.M, re.S, re.I, re.I | re.A, re.A, re.X | re.M])
        try:
            p = re.compile(pat, fl)
        except Exception as e:  # noqa: BLE001
            cases.append({"pattern": pat, "flags": int(fl), "error": exc(e)})
            continue
        subjects = ["".join(rng.choice("abcA\n x\u00e9\u212a\u017f_1") for _ in range(rng.randint(0, 8))) for _ in range(3)]
        for s in subjects:
            tmpl = "".join("<\\%d>" % g for g in range(1, p.groups + 1)) or "-"
            cases.append({
                "pattern": pat, "flags": int(fl), "subject": s,
                "search": spans(s, p.search(s)),
                "fullmatch": spans(s, p.fullmatch(s)),
                "finditer": [spans(s, m) for m in p.finditer(s)],
                "sub": p.sub(tmpl, s), "repl": tmpl,
            })
    return cases


def main():
    out = {"compile": [], "match": [], "template_errors": [], "split": [], "escape": [], "pos": []}
    for pat in ERRORS:
        out["compile"].append({"pattern": pat, "flags": 0, "result": compile_case(pat, 0)})
    for pat, fs, subjects, templates in MATCH:
        fl = flagval(fs)
        res = compile_case(pat, fl)
        out["compile"].append({"pattern": pat, "flags": fl, "result": res})
        if "exc" in res:
            continue
        p = re.compile(pat, fl)
        for s in subjects:
            case = {
                "pattern": pat, "flags": fl, "subject": s,
                "search": spans(s, p.search(s)),
                "match": spans(s, p.match(s)),
                "fullmatch": spans(s, p.fullmatch(s)),
                "finditer": [spans(s, m) for m in p.finditer(s)],
                "findall": p.findall(s),
                "subs": [],
            }
            for t in templates:
                for count in (0, 1):
                    try:
                        r, n = p.subn(t, s, count=count)
                        case["subs"].append({"repl": t, "count": count, "result": r, "n": n})
                    except Exception as e:  # noqa: BLE001
                        case["subs"].append({"repl": t, "count": count, "error": exc(e)})
            out["match"].append(case)
    for pat, reps in TEMPLATE_ERRORS:
        p = re.compile(pat)
        for t in reps:
            try:
                r = p.sub(t, "xab")
                out["template_errors"].append({"pattern": pat, "repl": t, "result": r})
            except Exception as e:  # noqa: BLE001
                out["template_errors"].append({"pattern": pat, "repl": t, "error": exc(e)})
    for pat, s, maxes in SPLIT:
        for mx in maxes:
            out["split"].append({"pattern": pat, "subject": s, "maxsplit": mx,
                                 "result": re.split(pat, s, maxsplit=mx)})
    for s in ESCAPE:
        out["escape"].append({"s": s, "result": re.escape(s)})
    # pos/endpos
    for pat, s in [(r"^a", "baa"), (r"\ba", "ba a"), (r"a$", "aab"), (r"(?<=b)a", "ba"), (r"a\Z", "aa"),
                   (r"\w+", "日本語abc"), (r"$", "ab\n"), (r"(?<!a)b", "abb")]:
        p = re.compile(pat)
        for pos in range(0, len(s) + 1):
            for endpos in range(pos, len(s) + 1):
                out["pos"].append({
                    "pattern": pat, "subject": s, "pos": boff(s, pos), "endpos": boff(s, endpos),
                    "search": spans(s, p.search(s, pos, endpos)),
                    "match": spans(s, p.match(s, pos, endpos)),
                    "fullmatch": spans(s, p.fullmatch(s, pos, endpos)),
                })
    # Character classes over every code point (surrogates excluded: a Go
    # string cannot hold them), as ranges.
    classes = {}
    for name, pat in [("w", r"\w"), ("d", r"\d"), ("s", r"\s"), ("aw", r"(?a)\w"), ("ad", r"(?a)\d"),
                      ("as", r"(?a)\s"), ("dot", r".")]:
        p = re.compile(pat)
        rs = []
        start = None
        for cp in range(0x110000):
            ok = 0xD800 <= cp <= 0xDFFF or p.match(chr(cp)) is not None
            if ok and start is None:
                start = cp
            elif not ok and start is not None:
                rs.append([start, cp - 1])
                start = None
        if start is not None:
            rs.append([start, 0x10FFFF])
        classes[name] = rs
    out["classes"] = classes
    # Case-insensitive matching: for every cased character, which of its
    # case relatives (and _EXTRA_CASES) a literal, a one-character set
    # and a backreference match.
    ic = []
    for cp in range(0x110000):
        if 0xD800 <= cp <= 0xDFFF:
            continue
        c = chr(cp)
        cands = set()
        for f in (str.lower, str.upper, str.title, str.casefold):
            for x in f(c):
                cands.add(x)
        if len(cands) <= 1 and c == c.lower() == c.upper():
            continue
        cands.add(c)
        from re._casefix import _EXTRA_CASES
        lo = ord(c.lower()[0])
        for k in _EXTRA_CASES.get(lo, ()):
            cands.add(chr(k))
        cands = sorted(x for x in cands if not 0xD800 <= ord(x) <= 0xDFFF)
        lit = re.compile("(?i)" + re.escape(c))
        cls = re.compile("(?i)[" + re.escape(c) + "]")
        alit = re.compile("(?ai)" + re.escape(c))
        bref = re.compile("(?i)(" + re.escape(c) + ")\\1")
        ic.append({
            "c": c,
            "cands": "".join(cands),
            "lit": "".join(x for x in cands if lit.fullmatch(x)),
            "cls": "".join(x for x in cands if cls.fullmatch(x)),
            "alit": "".join(x for x in cands if alit.fullmatch(x)),
            "bref": "".join(x for x in cands if bref.fullmatch(c + x)),
        })
    out["ignorecase"] = ic
    out["fuzz"] = fuzz()
    with open(sys.argv[1], "w") as f:
        json.dump(out, f, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
        f.write("\n")


if __name__ == "__main__":
    main()
