"""Generates plugin_names.json for TestEveryBuiltinPlugin: the names of
every ansible.builtin filter and test, Jinja2's (jinja2.defaults) and
ansible-core's (its plugins' FilterModule and TestModule maps). Test-data
tooling only; run it with the Python ansible-core is installed in.

Usage: python3 plugin_names.py plugin_names.json
"""
import importlib, json, pkgutil, sys, warnings

warnings.simplefilter("ignore")

import jinja2.defaults
import ansible.plugins.filter, ansible.plugins.test

filters, tests = set(jinja2.defaults.DEFAULT_FILTERS), set(jinja2.defaults.DEFAULT_TESTS)
for pkg, attr, names in ((ansible.plugins.filter, "FilterModule", filters), (ansible.plugins.test, "TestModule", tests)):
    for m in pkgutil.iter_modules(pkg.__path__):
        mod = importlib.import_module(pkg.__name__ + "." + m.name)
        if hasattr(mod, attr):
            plugins = getattr(mod, attr)()
            names |= set(plugins.filters() if attr == "FilterModule" else plugins.tests())

with open(sys.argv[1], "w") as f:
    json.dump({"filters": sorted(filters), "tests": sorted(tests)}, f, indent=1)
    f.write("\n")
