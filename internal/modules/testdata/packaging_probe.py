"""Ground truth for pep440.go / pep508.go from real `packaging` releases.

Usage: python3 packaging_probe.py DIR...

Each DIR holds one packaging release (pip install --target DIR
packaging==X; releases before 22.0 need a Python with distutils). Prints
packaging_ground_truth.json: for each requirement string, Requirement's
verdict (valid, str(), canonical name, bool(specifier)) and, for each
(specifier, version), SpecifierSet.contains(version, prereleases=True),
each grouped by the releases that agree.
"""
import json
import re
import subprocess
import sys

REQS = [
    "foo", " foo >= 1.0 ", "Foo_Bar[b,a]>=1.0,!=1.5,<2", "foo==1.0.*", "foo>=1.0.*", "foo~=1.4.5",
    "foo~=1", "foo===1.0-weird", "foo==1.0+local.1", "foo>=1.0+local", "foo (>=1.0)",
    "foo; python_version>'3.0'",
    "foo>=1;python_version>'3' and (os_name=='posix' or sys.platform == \"linux\")",
    "foo @ https://example.com/foo.tar.gz", "foo@https://example.com/foo.tar.gz ; python_version > '3'",
    "foo @ https://example.com/foo.tar.gz;python_version > '3'",
    "foo @ https://example.com/foo.tar.gz ",
    "foo @ file:///tmp/x.whl", "foo @ ./x.whl", "foo @ ",
    "git+https://github.com/x/y.git#egg=y", "./local/path", "foo>=1.0a1", "foo>=v1.0",
    "foo==1.0,==1.0.0", "foo>=1.0,>=1.0", "foo<=1.0.post1", "foo!=1.0.*", "foo>bar", "foo[bar",
    "-r requirements.txt", "foo ; extra == 'Test_X'", "foo; python_implementation == 'CPython'",
    "foo>=1.0, <2.0", "foo; 'linux' in sys_platform", "foo; os.name not in 'nt'", "FOO.bar-baz",
    "foo[ x , y ]", "foo>=1.0 # comment", "foo; python_version == \"3.1'\"", "foo\n",
    "foo; python_version>='3' ", "foo==1.0.0.0,==1", "foo; (python_version>'3')",
    "foo; ((os_name=='a' and os_name=='b') or os_name=='c') and os_name=='d'",
    "foo; os_name=='a' and (os_name=='b' or os_name=='c')",
    "foo; (os_name=='a')", "foo; ((os_name=='a'))",
    "foo==1.0.dev456", "foo==1!2.0", "foo==V1.0", "foo==1.0-1", "foo==1.0_rc_1",
    "foo; platform.python_implementation=='x'", "foo; implementation_name=='x' and extras=='y'",
    "foo; 'Foo_Bar' in extras", "foo; extra == 'a.b'", "foo; 'X_Y' == extra",
    "foo; dependency_groups == 'x'", "foo; python_full_version >= '3.8.1'",
    "foo; platform_release == 'x' and platform_system=='Linux' and platform_version=='1' and platform_machine=='x86_64'",
    "foo; os.name=='a'", "foo; sys.platform=='a'", "foo; platform.version=='a'", "foo; platform.machine=='a'",
    "foo; platform_python_implementation=='x'", "foo; python_version=='3.8' or",
    "foo;", "foo ;", "foo; ", "foo>=1.0;", "foo[]", "foo[ ]", "foo[a,]", "foo[a b]",
    "foo==", "foo >=1.0 <2", "foo>=1.0,", "foo,>=1.0", "foo>=1.0,,<2", "foo>=1.0 ,<2",
    "foo==1.0.*,!=1.0.1", "foo~=1.0.0rc1", "foo~=1.0.post1", "foo~=1.0.dev1", "foo~=1.0+l",
    "foo==1.0a", "foo==1.0alpha1", "foo==1.0preview2", "foo==1.0c1", "foo==1.0-r1", "foo==1.0rev3",
    "foo==1.0post", "foo==1.0.post", "foo==1.0dev", "foo==1.0-dev-1", "foo==1.0+ABC",
    "foo==1.0+abc_def-1.2", "foo==1.0.*+x", "foo==1.0a1.*", "foo==1.*.0", "foo==*",
    "foo<1.0.*", "foo===", "foo=== 1.0", "foo===1.0 ; os_name=='a'", "foo===foo bar",
    "foo (==1.0", "foo ==1.0)", "foo(==1.0)", "foo ( == 1.0 , < 2 )", "foo (>=1.0); os_name=='a'",
    "-e foo", "foo-", "_foo", "foo_", "f", "1foo", "foo.", "foo bar", "foo\tbar", "foo\t>=1",
    "\tfoo", "foo; os_name == 'a'\n", "foo==1.0\n", "foo; os_name=='a' and", "foo; os_name='a'",
    "foo; os_name ==", "foo; os_name == a", "foo; os_name == 'a' and(os_name=='b')",
    "foo; os_name=='a'and os_name=='b'", "foo; os_name in'a'", "foo; 'a'in os_name",
    "foo; os_name not  in 'a'", "foo; os_name notin 'a'", "foo; python_version ~= '3.8'",
    "foo; python_version === '3.8'", "foo; '3.8' < python_version", "foo; os_name == 'a\"b'",
    "foo; os_name == \"a'b\"", "foo; os_name == ''", "foo; os_name == '\\x41'", "foo; os_name == 'a\\\\b'",
    "foo; os_name == u'a'", "foo; os_name == b'a'", "foo; os_name == 'a' 'b'",
    "foo @ https://x/y.whl; os_name=='a'", "foo@ https://x/y.whl", "foo@https://x/y.whl",
    "foo[x] @ https://x/y.whl", "foo>=1 @ https://x/y.whl", "foo @ https://x/y.whl >=1",
    "foo==1.0.0+local", "foo<=1.0", "foo>1.0", "foo<1.0", "foo!=1.0", "foo==01.02", "foo==1.0.0",
    "foo>=1.0.0.0", "foo~=2.0", "foo~=2.0.0", "foo==1.0.post1.dev2", "foo==1.0a1.post2.dev3+x",
    "foo===1.0", "foo===1.0+abc", "foo=1.0", "foo<>1.0", "foo=>1.0", "foo>=1.0.*.0",
    "foo>==1.0", "foo>=-1", "foo>=1.0-", "foo>=1.0.", "foo>=.1",
    "foo ;os_name=='a'", "foo  ;  os_name=='a'  ", " foo", "foo ", "foo  ",
    "ünicode", "foo; os_name=='ü'", "foo==1.0+ü", "foo==١", "foo; python_version>'3.0' and python_version<'4' or os_name=='x'",
]

CONTAINS = [
    ("==1.0", ["1.0", "1.0.0", "1", "1.0+local", "1.0a1", "1.0.post1", "1.0.dev1", "01.0", "1.0-1", "v1.0", "1.0.0.0.1"]),
    ("==1.0.*", ["1.0", "1.0.5", "1.0a1", "1.0.post1", "1.1", "1.0+l", "1", "1.0.0.dev1", "1.0.dev1", "1.01"]),
    ("==1.*", ["1", "1.9", "2.0", "1.0a1", "0.9"]),
    ("!=1.0.*", ["1.0", "1.1", "1.0.5"]),
    ("!=1.0", ["1.0", "1.0+x", "1.1"]),
    ("==1.0+local", ["1.0+local", "1.0", "1.0+other", "1.0+LOCAL"]),
    ("!=1.0+local", ["1.0+local", "1.0"]),
    (">=1.0", ["1.0", "0.9", "1.0a1", "1.0+x", "2.0rc1", "1.0.dev0", "1.0.post1", "0.9+x"]),
    ("<=1.0", ["1.0", "1.0+x", "1.0.post1", "1.0a1", "1.1"]),
    (">1.0", ["1.0", "1.0.1", "1.0.post1", "1.0+x", "1.1a1", "1.0.post1.dev1", "1.0.1+x", "1.0.0.1", "1.0.dev1+x", "1.0.post1+x"]),
    (">1.0.post1", ["1.0.post2", "1.0.post1", "1.0.post1+x", "1.1"]),
    ("<1.0", ["0.9", "1.0", "1.0a1", "1.0.dev1", "0.9a1", "0.9+x", "1.0+x", "1.0rc1+x"]),
    ("<1.0a1", ["1.0a0", "1.0.dev1", "0.9"]),
    ("<1.0.0.0", ["1.0rc1", "0.9"]),
    ("~=1.4.5", ["1.4.5", "1.4.9", "1.5.0", "1.4.4", "1.4.5a1", "1.4.5.post1", "1.4.5+x"]),
    ("~=1.4", ["1.4", "1.9", "2.0", "1.3", "2.0a1"]),
    ("~=1.4.5a1", ["1.4.5a1", "1.4.5", "1.4.6", "1.5"]),
    ("~=1.4.post1", ["1.4.post1", "1.4", "1.5", "2.0"]),
    ("~=2.2.0", ["2.2.0", "2.2.1", "2.3"]),
    ("===1.0", ["1.0", "1.0.0", "1.0+x", "foo"]),
    ("===foo", ["foo", "FOO", "bar"]),
    ("===1.0-weird", ["1.0-weird", "1.0.post0"]),
    ("===V1.0", ["v1.0", "1.0"]),
    (">=1.0,<2.0", ["1.5", "2.0", "0.5", "2.0a1", "1.9.9+x"]),
    ("", ["1.0", "1.0a1", "weird", "1.0+x"]),
    (">=1.0", ["weird", "1.0-weird", "1.0 ", " 1.0", "1.0.0-alpha", "1.0beta2", "1!1.0", "0!2.0"]),
    ("==1!1.0", ["1!1.0", "1.0"]),
    (">1!0.5", ["2.0", "1!1.0"]),
    ("==1.0.dev1", ["1.0dev1", "1.0-dev-1", "1.0.dev1"]),
    ("==1.0.post1", ["1.0-1", "1.0post1", "1.0.r1", "1.0rev1", "1.0-post-1"]),
    ("==1.0rc1", ["1.0c1", "1.0pre1", "1.0preview1", "1.0-rc.1"]),
    ("==1.0a0", ["1.0a", "1.0alpha", "1.0.a"]),
    (">=1.0.*", ["1.0", "1.1", "0.9", "1.0.5"]),
    (">=1.0+local", ["1.0", "1.1"]),
    ("~=1", ["1", "2"]),
    (">=v1.0", ["1.0"]),
    ("<=1.0+x", ["1.0"]),
    ("==1.0,==1.0.0", ["1.0", "1.1"]),
]


def probe(s):
    from packaging.requirements import Requirement
    try:
        r = Requirement(s)
    except Exception:  # InvalidRequirement (a ValueError) or other
        return [False]
    return [True, str(r), re.sub(r"[-_.]+", "-", r.name).lower(), bool(r.specifier)]


def contains(spec, v):
    from packaging.requirements import Requirement
    try:
        r = Requirement("x" + spec)
    except Exception:
        return "invalid-req"
    try:
        return r.specifier.contains(v, prereleases=True)
    except Exception as e:
        return "raise:" + type(e).__name__


def run_one(pkdir):
    sys.path.insert(0, pkdir)
    import packaging
    return {"version": packaging.__version__,
            "reqs": [probe(s) for s in REQS],
            "contains": [contains(spec, v) for spec, vs in CONTAINS for v in vs]}


def group(results):
    """[(version, value)] -> [[value, [versions...]], ...] in first-seen order."""
    out = []
    for version, value in results:
        for entry in out:
            if entry[0] == value:
                entry[1].append(version)
                break
        else:
            out.append([value, [version]])
    return out


def main():
    if sys.argv[1:2] == ["--one"]:
        json.dump(run_one(sys.argv[2]), sys.stdout)
        return
    runs = [json.loads(subprocess.check_output([sys.executable, __file__, "--one", d])) for d in sys.argv[1:]]
    pairs = [(spec, v) for spec, vs in CONTAINS for v in vs]
    out = {
        "reqs": [[s, group([(r["version"], r["reqs"][i]) for r in runs])] for i, s in enumerate(REQS)],
        "contains": [[spec, v, group([(r["version"], r["contains"][i]) for r in runs])]
                     for i, (spec, v) in enumerate(pairs)],
    }
    json.dump(out, sys.stdout, ensure_ascii=False, indent=None, separators=(",", ":"))


main()
