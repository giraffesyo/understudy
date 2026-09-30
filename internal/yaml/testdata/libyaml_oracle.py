"""Generates libyaml.json for TestLibyamlCorpus: the node tree (or the error)
libyaml composes for each input, as ansible-core loads YAML (PyYAML with
libyaml, CBaseLoader.get_single_node). Test-data tooling only.

Usage: python3 libyaml_oracle.py libyaml_seeds.txt libyaml.json 3000 11
libyaml_seeds.txt holds YAML snippets separated by lines of '#####'; the run
adds that many random mutations of them (the last argument seeds them).
"""
import json, random, sys
import yaml
from yaml.nodes import ScalarNode, SequenceNode, MappingNode

STYLE = {None: 'plain', '': 'plain', "'": 'single', '"': 'double', '|': 'literal', '>': 'folded'}
DEFAULT = {'tag:yaml.org,2002:str', 'tag:yaml.org,2002:seq', 'tag:yaml.org,2002:map'}


def dump(node, out, depth=0, seen=None):
    pos = '%d:%d' % (node.start_mark.line + 1, node.start_mark.column + 1)
    tag = '' if node.tag in DEFAULT else ' <' + node.tag + '>'
    pad = ' ' * depth
    if depth > 40:
        out.append(pad + 'DEEP')
        return
    if isinstance(node, ScalarNode):
        out.append('%s=VAL %s%s %s %s' % (pad, pos, tag, STYLE[node.style], json.dumps(node.value)))
    elif isinstance(node, SequenceNode):
        out.append('%s+SEQ %s%s %s' % (pad, pos, tag, 'flow' if node.flow_style else 'block'))
        for c in node.value:
            dump(c, out, depth + 1)
    else:
        out.append('%s+MAP %s%s %s' % (pad, pos, tag, 'flow' if node.flow_style else 'block'))
        for k, v in node.value:
            dump(k, out, depth + 1)
            dump(v, out, depth + 1)


def run(src):
    loader = yaml.CBaseLoader(src)
    try:
        node = loader.get_single_node()
    except yaml.MarkedYAMLError as e:
        m = e.problem_mark
        return ['ERR %d:%d %s|%s' % (m.line + 1, m.column + 1, e.context or '', e.problem or '')]
    except yaml.YAMLError as e:
        return ['OTHER ' + type(e).__name__ + ' ' + str(e).split('\n')[0]]
    except RecursionError:
        return ['RECURSION']
    finally:
        loader.dispose()
    if node is None:
        return ['EMPTY']
    out = []
    try:
        dump(node, out)
    except RecursionError:
        return ['RECURSION']
    return out


MUT = [':', ': ', '-', '- ', '\t', '"', "'", '[', ']', '{', '}', ',', '&a', '*a', '!', '!!', '|', '>', '\n', ' ', '  ', '#', '?', '? ', '%', '@', '`', '\\', '---', '...', '\n- ', '\n  ', 'x: ', '&', '*', '|-', '>+2', '\r\n', '\r', '\u2028', '\x85', 'é', '\t\t', '  - ', ': |', '"\\', "''", '{{ x }}', '\n\t']


def mutate(rng, s):
    n = rng.randint(1, 3)
    for _ in range(n):
        op = rng.random()
        i = rng.randint(0, len(s))
        if op < 0.6 or not s:
            s = s[:i] + rng.choice(MUT) + s[i:]
        elif op < 0.9:
            j = min(len(s), i + rng.randint(1, 3))
            s = s[:i] + s[j:]
        else:
            j = min(len(s), i + rng.randint(1, 3))
            s = s[:i] + rng.choice(MUT) + s[j:]
    return s


def main():
    seeds = open(sys.argv[1]).read().split('\n#####\n')
    nmut = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    rng = random.Random(int(sys.argv[4]) if len(sys.argv) > 4 else 42)
    cases = []
    seen = set()
    for s in seeds:
        if s not in seen:
            seen.add(s)
            cases.append(s)
    for _ in range(nmut):
        s = mutate(rng, rng.choice(seeds))
        if s not in seen:
            seen.add(s)
            cases.append(s)
    res = [{'in': c, 'out': run(c)} for c in cases]
    json.dump(res, open(sys.argv[2], 'w'), indent=0, ensure_ascii=False)
    print(len(res), 'cases,', sum(1 for r in res if r['out'][0].startswith('ERR')), 'errors')


main()
