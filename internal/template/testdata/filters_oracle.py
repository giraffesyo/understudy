"""Generates filters.json for TestFiltersMatchAnsible: what ansible-core's
templar renders for templates using the filters and tests understudy
implements after Jinja2's and ansible-core's own (items, urlize, pprint,
product, to_datetime, the path and URI tests, ...): the value (as JSON,
datetimes as their isoformat()), or the error's message. Test-data
tooling only; run it with the Python ansible-core is installed in.

Usage: python3 filters_oracle.py filters.json
"""
import json, sys, warnings

warnings.simplefilter("ignore")

from ansible.template import Templar
from ansible.parsing.dataloader import DataLoader
from ansible._internal._datatag._tags import TrustedAsTemplate

VARS = {
    "d": {"b": 1, "a": 2},
    "users": [
        {"name": "alice", "groups": ["wheel", "adm"], "info": {"keys": ["k1"]}},
        {"name": "bob", "groups": [], "info": {"keys": ["k2", "k3"]}},
    ],
    "byname": {"x": {"id": 1, "n": "a"}, "y": {"id": 2, "n": "b"}},
    "long": "aaaa bbbb cccc dddd eeee ffff gggg hhhh iiii jjjj kkkk llll mmmm nnnn oooo pppp qqqq rrrr ssss tttt",
    "res": {"changed": False, "failed": False},
}

TEMPLATES = [
    # items, attr
    "{{ d | items }}", "{{ d | items | list }}", "x{{ d | items | list }}", "{{ 'abc' | items }}", "{{ [] | items }}",
    "{{ d | attr('nope') is undefined }}", "{{ 'abc' | attr('nope') is defined }}",
    # escape, safe, forceescape, tojson
    "{{ '<a href=\"x\">&\\'</a>' | e }}", "{{ '<a>' | escape | e }}", "{{ ('<a>' | e) | forceescape }}", "{{ '<b>' | safe }}",
    "{{ 'x' ~ ('<' | e) }}", "{{ ('<' | e) + '<' }}", "{{ '<' + ('<' | e) }}", "{{ ('<a>' | e) | upper }}",
    "{{ 3 | e }}", "{{ none | e }}", "{{ ['<', 1] | e }}",
    "{{ d | tojson }}", "{{ {'k': \"<it's & that>\"} | tojson }}", "{{ [1, {'b': 2, 'a': [none, true]}] | tojson(indent=2) }}",
    "{{ 'x' | tojson(indent=0) }}", "{{ ['\u00e9'] | tojson }}",
    # pprint
    "{{ d | pprint }}", "{{ long | pprint }}", "{{ (long ~ ' ' ~ long) | pprint }}", "{{ 'a\\nb' | pprint }}", "{{ 3.0 | pprint }}",
    "{{ none | pprint }}", "{{ range(40) | list | pprint }}", "{{ ('<a>' | e) | pprint }}", "{{ (long ~ '\\n' ~ long ~ '\\n') | pprint }}",
    # urlencode, urldecode
    "{{ 'a b&c/d?e=f' | urlencode }}", "{{ d | urlencode }}", "{{ [['a', 'b c'], ['d/e', 1]] | urlencode }}", "{{ 3 | urlencode }}",
    "{{ {'a b': 'c/d', 'x': none} | urlencode }}", "{{ '\u00e9~' | urlencode }}", "{{ ['abc'] | urlencode }}", "{{ [1] | urlencode }}",
    "{{ 'a%20b+c%zz%41' | urldecode }}", "{{ '%C3%A9%C3' | urldecode }}", "{{ 3 | urldecode }}",
    # filesizeformat, wordcount, striptags, xmlattr
    "{{ 0 | filesizeformat }}", "{{ 1 | filesizeformat }}", "{{ 999 | filesizeformat }}", "{{ 1000 | filesizeformat }}",
    "{{ 12345678 | filesizeformat }}", "{{ 1024 | filesizeformat(true) }}", "{{ '1048576' | filesizeformat(binary=true) }}",
    "{{ 1e30 | filesizeformat }}", "{{ -5 | filesizeformat }}", "{{ 'x' | filesizeformat }}",
    "{{ 'Hello world, foo_bar 12 \u00e9t\u00e9' | wordcount }}", "{{ '' | wordcount }}", "{{ 123 | wordcount }}",
    "{{ 'a <b>bold</b>  &amp; <!-- c <d> --> e &lt;f&gt; &raquo;' | striptags }}", "{{ '<a' | striptags }}",
    "{{ {'class': 'x y', 'id': 3, 'n': none, 'v': '<\"&>'} | xmlattr }}", "{{ {'a': 1} | xmlattr(false) }}", "{{ {} | xmlattr }}",
    "{{ {'a b': 1} | xmlattr }}", "{{ {'a/b': 1} | xmlattr }}",
    # urlize
    "{{ 'see http://x.com now' | urlize }}", "{{ 'go to www.example.com, or (https://x.org/a). mail me@x.com' | urlize }}",
    "{{ 'http://averyveryverylongurl.com/path' | urlize(10, true, '_blank') }}", "{{ 'x <http://a.com> y' | urlize }}",
    "{{ 'mailto:a@b.com mailto:bad @x a@b@c example.org' | urlize }}", "{{ 'ftp://x tel:123' | urlize(extra_schemes=['ftp://', 'tel:']) }}",
    "{{ 'https://127.0.0.1:8080/x?y#z http://[::1]/' | urlize(rel='me') }}", "{{ 'x' | urlize(extra_schemes=['bad']) }}",
    "{{ '(www.a.com)' | urlize }}", "{{ 'a.b.c foo.com bar.net' | urlize }}",
    # itertools
    "{{ [1, 2] | product([3, 4]) }}", "{{ [1, 2] | product('ab', repeat=2) | length }}", "{{ [1, 2] | product(repeat=-1) }}",
    "{{ [1, 2] | product(3) }}", "{{ [] | product([1]) }}", "{{ [1, 2] | product }}",
    "{{ [1, 2, 3, 4] | combinations(2) }}", "{{ 'abc' | combinations(3) }}", "{{ [1, 2] | combinations(5) }}",
    "{{ [1, 2] | combinations(-1) }}", "{{ [1, 2] | combinations('a') }}", "{{ [1, 2] | combinations(1.0) }}",
    "{{ [1, 2, 3] | permutations }}", "{{ [1, 2, 3] | permutations(2) }}", "{{ [1, 2] | permutations(0) }}", "{{ [1, 2] | permutations(-2) }}",
    "{{ 'ab' | zip_longest('xyz', [1]) }}", "{{ [1] | zip_longest([2, 3], fillvalue='-') }}", "{{ [1] | zip_longest(1) }}",
    # paths
    "{{ '/a/./b/../c//d/' | normpath }}", "{{ '//a' | normpath }}", "{{ '///a/b' | normpath }}", "{{ '' | normpath }}",
    "{{ '../x/..' | normpath }}", "{{ 'a/../../b' | normpath }}", "{{ 3 | normpath }}",
    "{{ '/etc/ssh/sshd_config' | relpath('/etc/pki') }}", "{{ '/a/b' | relpath('/a/b') }}", "{{ '/' | relpath('/a/b') }}",
    "{{ '' | relpath('/') }}", "{{ 3 | relpath('/') }}",
    "{{ ['/usr/lib', '/usr/local/lib', '/usr/lib/x'] | commonpath }}", "{{ ['a/b/./c', 'a/b/d'] | commonpath }}",
    "{{ ['/usr/lib', 'usr'] | commonpath }}", "{{ [] | commonpath }}", "{{ '/usr' | commonpath }}", "{{ ['/a', 3] | commonpath }}",
    "{{ 'C:\\\\Users\\\\me\\\\file.txt' | win_basename }}", "{{ 'C:\\\\Users\\\\me\\\\file.txt' | win_dirname }}",
    "{{ 'C:/Users/me/' | win_dirname }}", "{{ 'C:Users' | win_splitdrive }}", "{{ 'C:\\\\Users' | win_splitdrive }}",
    "{{ 'x/y\\\\z' | win_basename }}", "{{ 3 | win_basename }}",
    # to_uuid
    "{{ 'abc' | to_uuid }}", "{{ 'abc' | to_uuid(namespace='6ba7b810-9dad-11d1-80b4-00c04fd430c8') }}",
    "{{ 'abc' | to_uuid(namespace='{6BA7B810-9DAD-11D1-80B4-00C04FD430C8}') }}", "{{ 'abc' | to_uuid(namespace='zz') }}",
    "{{ 'abc' | to_uuid(namespace='zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz') }}", "{{ 3 | to_uuid }}", "{{ [1] | to_uuid }}",
    # subelements, rekey_on_member
    "{{ users | subelements('groups') }}", "{{ users | subelements('info.keys') }}", "{{ users | subelements(['info', 'keys']) }}",
    "{{ users | subelements('nope', skip_missing=true) }}", "{{ users | subelements('name') }}", "{{ 'x' | subelements('name') }}",
    "{{ users | subelements(3) }}", "{{ byname | subelements('n') }}",
    "{{ byname | rekey_on_member('n') }}", "{{ [{'n': 'a'}, {'n': 'a', 'z': 1}] | rekey_on_member('n') }}",
    "{{ [{'n': 'a'}, {'n': 'a', 'z': 1}] | rekey_on_member('n', duplicates='overwrite') }}", "{{ [1] | rekey_on_member('q') }}",
    "{{ 'abc' | rekey_on_member('q') }}", "{{ [] | rekey_on_member('q', duplicates='x') }}",
    # to_datetime and datetimes
    "{{ '2020-01-02 03:04:05' | to_datetime }}", "x{{ '2020-01-02 03:04:05' | to_datetime }}",
    "{{ ['2020-01-02' | to_datetime('%Y-%m-%d')] | string }}", "{{ ('2020-01-02 03:04:05' | to_datetime).year }}",
    "{{ (('2020-01-02 03:04:05' | to_datetime) - ('2019-12-31 00:00:00' | to_datetime)).days }}",
    "{{ (('2020-01-02 03:04:05' | to_datetime) - ('2019-12-31 00:00:00' | to_datetime)).total_seconds() }}",
    "{{ (('2019-12-31 00:00:00' | to_datetime) - ('2020-01-02 03:04:05' | to_datetime)) | string }}",
    "{{ (('2019-12-31 00:00:00' | to_datetime) - ('2020-01-02 03:04:05' | to_datetime)).seconds }}",
    "{{ ('2020-01-02' | to_datetime('%Y-%m-%d')) < ('2020-01-03' | to_datetime('%Y-%m-%d')) }}",
    "{{ ('2020-01-02' | to_datetime('%Y-%m-%d')) == ('2020-01-02 00:00:00' | to_datetime) }}",
    "{{ ('2020-01-02' | to_datetime('%Y-%m-%d')).strftime('%d/%m/%y %a %b %j %p') }}",
    "{{ '2021-06-01T12:30:00.25+0200' | to_datetime('%Y-%m-%dT%H:%M:%S.%f%z') }}",
    "{{ '2021-06-01T12:30:00Z' | to_datetime('%Y-%m-%dT%H:%M:%S%z') }}",
    "{{ '2021-06-01 -05:30' | to_datetime('%Y-%m-%d %z') | string }}",
    "{{ 'Jun 1 2021 1:05PM' | to_datetime('%b %d %Y %I:%M%p') }}", "{{ 'monday JUNE 07 21' | to_datetime('%A %B %d %y') }}",
    "{{ '12/31/99 23:59' | to_datetime('%D %R') }}", "{{ '2021 100' | to_datetime('%Y %j') }}", "{{ '1  2   2021' | to_datetime('%m %d %Y') }}",
    "{{ 'bad' | to_datetime }}", "{{ '2020-01-02 03:04:05x' | to_datetime }}", "{{ '2020-02-30' | to_datetime('%Y-%m-%d') }}",
    "{{ '2020' | to_datetime('%q') }}", "{{ '2020' | to_datetime('%Y%') }}", "{{ 3 | to_datetime }}", "{{ 'x' | to_datetime(3) }}",
    "{{ ('2020-01-02 03:04:05' | to_datetime) | pprint }}", "{{ ('2020-01-02 03:04:05' | to_datetime) | to_json }}",
    "{{ ('2020-01-02 03:04:05' | to_datetime) | type_debug }}", "{{ (('2020-01-02' | to_datetime('%Y-%m-%d')) - ('2020-01-01' | to_datetime('%Y-%m-%d'))) | type_debug }}",
    "{{ ('2020-01-02 03:04:05.000001' | to_datetime('%Y-%m-%d %H:%M:%S.%f')).microsecond }}",
    # tests
    "{{ 'abc' is lower }} {{ 'aBc' is lower }} {{ '123' is lower }} {{ 'ABC 1' is upper }} {{ 3 is upper }} {{ '\u01c5' is upper }}",
    "{{ '/etc' is abs }} {{ 'etc' is is_abs }} {{ '' is abs }}", "{{ 3 is abs }}",
    "{{ (0.0 / 1) is nan }} {{ 'nan' is nan }} {{ ('nan' | float) is isnan }} {{ 3 is nan }} {{ none is nan }}",
    "{{ [1, 2] is issubset([1, 2, 3]) }} {{ [1, 2, 3] is issuperset([1]) }} {{ 'ab' is issubset('abc') }}",
    "{{ [[1]] is issubset([[1]]) }}", "{{ [{}] is issuperset([1]) }}", "{{ 3 is issubset([1]) }}",
    "{{ 'upper' is filter }} {{ 'ansible.builtin.upper' is filter }} {{ 'nope' is filter }} {{ 'url' is test }} {{ 3 is test }} {{ 'url' is filter }}",
    "{{ [1] is filter }}", "{{ {} is test }}",
    "{{ 'x' is escaped }} {{ 'x' | e is escaped }} {{ 'x' | forceescape | safe is escaped }}",
    "{{ res is successful }} {{ res is reachable }} {{ res is unreachable }} {{ res is timedout }} {{ res is change }}",
    "{{ {'timedout': {'period': 3}} is timedout }} {{ {'timedout': {}} is timedout }} {{ {'unreachable': 1} is reachable }}",
    "{{ {'timedout': 1} is timedout }}", "{{ 'x' is reachable }}", "{{ {'started': 0, 'finished': 1} is started }} {{ {'finished': 1} is finished }}",
    "{{ 'http://x.com/a' is url }} {{ 'x.com' is url }} {{ 'file:///etc' is url }} {{ 'mailto:me@x' is uri }} {{ 'urn:isbn:1' is urn }}",
    "{{ 'foo' is uri }} {{ 3 is uri }} {{ '' is uri }} {{ 'HTTP://x' is url(['http']) }} {{ 'https://x' is uri(['http']) }}",
    "{{ 'https://x' is url(schemes=['https']) }} {{ 'http://[x' is url }} {{ 'git+ssh://h/r' is url }} {{ 'urn:x' is urn }} {{ 'URN:x' is urn }}",
    "{{ [1, 2, 3, 2] | select('==', 2) | list }} {{ [1, 2, 3] | select('>', 1) | list }} {{ [1, 2, 3] | reject('<=', 2) | list }}",
    "{{ [1, 2, 3] | select('!=', 2) | list }} {{ [1, 2, 3] | select('>=', 2) | list }} {{ [1, 2, 3] | select('<', 2) | list }}",
    "{{ 'x' is vault_encrypted }} {{ ('x' | vault('pw', salt='s')) is vault_encrypted }}",
    "{{ 'secret' | vault('pw', salt='saltsaltsaltsalt') }}", "{{ 'secret' | vault('pw', salt='s', vault_id='myid') }}",
    "{{ 'secret' | vault('pw', salt='s', vault_id='default') }}",
    "{{ 'secret' | vault('pw') | unvault('pw') }}", "{{ 'plain' | unvault('pw') }}", "{{ 3 | vault('pw') }}", "{{ 'x' | vault(3) }}",
    "{{ 'x' | vault('pw', salt='') }}", "{{ 3 | unvault('pw') }}", "{{ 'x' | unvault(none) }}",
]

templar = Templar(loader=DataLoader(), variables=VARS)


def render(tpl):
    try:
        value = templar.template(TrustedAsTemplate().tag(tpl))
    except Exception as ex:
        return {"error": str(ex)}
    return {"value": json.loads(json.dumps(value, default=lambda o: o.isoformat()))}


out = {"vars": VARS, "cases": [dict(template=t, **render(t)) for t in TEMPLATES]}
with open(sys.argv[1], "w") as f:
    json.dump(out, f, indent=1, ensure_ascii=False)
    f.write("\n")
