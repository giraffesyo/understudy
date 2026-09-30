"""Print the argument specs of Ansible modules as JSON.

Usage: python argspec.py NAME [NAME ...]

For each module name (short, FQCN or a routed legacy name) the module is
resolved through ansible-core's plugin loader, so collection routing and
redirects apply exactly as ansible-playbook applies them. The spec comes
from the module itself: its main() is run with AnsibleModule patched to
capture the argument_spec it is constructed with (the authoritative list,
including undocumented options). Modules that never construct an
AnsibleModule (action-only plugins documented by a stub module such as
template or debug) fall back to the options of their DOCUMENTATION, with
doc fragments merged. Output: {name: {"resolved", "path", "via",
"params": {param: [aliases]}, "error"}}.
"""

import contextlib
import importlib.util
import io
import json
import sys


class _Captured(Exception):
    pass


def _options_from_spec(spec):
    return {name: sorted(opt.get("aliases") or []) for name, opt in spec.items()}


def _capture_argument_spec(path):
    from ansible.module_utils import basic

    captured = {}
    orig_init = basic.AnsibleModule.__init__

    def fake_init(self, argument_spec=None, *args, **kwargs):
        spec = dict(argument_spec or {})
        if kwargs.get("add_file_common_args"):
            for k, v in basic.FILE_COMMON_ARGUMENTS.items():
                spec.setdefault(k, v)
        captured["spec"] = spec
        raise _Captured()

    basic.AnsibleModule.__init__ = fake_init
    try:
        spec = importlib.util.spec_from_file_location("ansible_module_under_audit", path)
        mod = importlib.util.module_from_spec(spec)
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            try:
                spec.loader.exec_module(mod)
                main = getattr(mod, "main", None) or getattr(mod, "run_module", None)
                if main is not None:
                    main()
            except (_Captured, SystemExit):
                pass
            except Exception:
                pass
    finally:
        basic.AnsibleModule.__init__ = orig_init
    return captured.get("spec")


def _options_from_doc(path):
    from ansible.plugins.loader import fragment_loader
    from ansible.utils.plugin_docs import get_docstring

    doc = get_docstring(path, fragment_loader)[0] or {}
    return {name: sorted(opt.get("aliases") or []) for name, opt in (doc.get("options") or {}).items()}


def audit(name):
    from ansible.plugins.loader import module_loader

    out = {"resolved": None, "path": None, "via": None, "params": None, "error": None}
    ctx = module_loader.find_plugin_with_context(name, check_aliases=True)
    if not ctx.resolved:
        out["error"] = "not found"
        return out
    out["resolved"] = ctx.resolved_fqcn
    out["path"] = ctx.plugin_resolved_path
    spec = None
    if out["path"] and out["path"].endswith(".py"):
        spec = _capture_argument_spec(out["path"])
    if spec:
        out["via"] = "argument_spec"
        out["params"] = _options_from_spec(spec)
        return out
    try:
        out["params"] = _options_from_doc(out["path"])
        out["via"] = "documentation"
    except Exception as e:  # pragma: no cover - reported, not fatal
        out["error"] = "%s: %s" % (type(e).__name__, e)
    return out


def main():
    import ansible
    from ansible.plugins.loader import init_plugin_loader

    init_plugin_loader()
    result = {"_ansible_version": ansible.__version__}
    for name in sys.argv[1:]:
        try:
            result[name] = audit(name)
        except Exception as e:
            result[name] = {"error": "%s: %s" % (type(e).__name__, e)}
    json.dump(result, sys.stdout, indent=1, sort_keys=True)


if __name__ == "__main__":
    main()
