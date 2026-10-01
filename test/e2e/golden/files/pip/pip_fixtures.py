"""Offline fixtures for the pip golden playbooks.

Usage: pip_fixtures.py DIR

Writes DIR/wheels (tiny pure-Python wheels: several versions and names
that exercise PEP 440 / PEP 503 normalization), DIR/proj and DIR/repo
(projects built by an in-tree, dependency-free PEP 517/660 backend; the
second is made a git repository by the playbook) and a requirements
file, so pip never needs the network.
"""
import base64
import hashlib
import os
import sys
import zipfile

WHEELS = [
    ("goldpkg", "1.0"), ("goldpkg", "1.5"), ("goldpkg", "2.0rc1"), ("goldpkg", "2.0"),
    ("Gold_Dash", "0.1"), ("goldlocal", "1.0+local.7"), ("goldextra", "1.0"),
    ("golddep", "3.0"),
]
REQUIRES = {"goldextra": ["golddep; extra == 'more'"]}
EPOCH = (2020, 1, 1, 0, 0, 0)


def record_hash(data):
    return "sha256=" + base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()


def write_wheel(directory, name, version, files):
    dist = "%s-%s.dist-info" % (name, version)
    meta = "Metadata-Version: 2.1\nName: %s\nVersion: %s\n" % (name, version)
    for req in REQUIRES.get(name, []):
        meta += "Provides-Extra: more\nRequires-Dist: %s\n" % req
    files = dict(files)
    files[dist + "/METADATA"] = meta
    files[dist + "/WHEEL"] = "Wheel-Version: 1.0\nGenerator: golden\nRoot-Is-Purelib: true\nTag: py3-none-any\n"
    filename = "%s-%s-py3-none-any.whl" % (name, version)
    records = []
    with zipfile.ZipFile(os.path.join(directory, filename), "w") as z:
        for path in sorted(files):
            data = files[path].encode()
            z.writestr(zipfile.ZipInfo(path, EPOCH), data)
            records.append("%s,%s,%d" % (path, record_hash(data), len(data)))
        records.append(dist + "/RECORD,,")
        z.writestr(zipfile.ZipInfo(dist + "/RECORD", EPOCH), "\n".join(records) + "\n")


BACKEND = '''"""A dependency-free PEP 517/660 build backend for the golden fixtures."""
import base64, hashlib, os, zipfile

NAME, VERSION = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "NAMEVER")).read().split()


def _wheel(directory, files):
    dist = "%s-%s.dist-info" % (NAME, VERSION)
    files = dict(files)
    files[dist + "/METADATA"] = "Metadata-Version: 2.1\\nName: %s\\nVersion: %s\\n" % (NAME, VERSION)
    files[dist + "/WHEEL"] = "Wheel-Version: 1.0\\nGenerator: golden\\nRoot-Is-Purelib: true\\nTag: py3-none-any\\n"
    filename = "%s-%s-py3-none-any.whl" % (NAME, VERSION)
    records = []
    with zipfile.ZipFile(os.path.join(directory, filename), "w") as z:
        for path in sorted(files):
            data = files[path].encode()
            z.writestr(path, data)
            digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
            records.append("%s,sha256=%s,%d" % (path, digest, len(data)))
        records.append(dist + "/RECORD,,")
        z.writestr(dist + "/RECORD", "\\n".join(records) + "\\n")
    return filename


def get_requires_for_build_wheel(config_settings=None):
    return []


get_requires_for_build_editable = get_requires_for_build_wheel


def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    return _wheel(wheel_directory, {NAME + "/__init__.py": "VERSION = %r\\n" % VERSION})


def build_editable(wheel_directory, config_settings=None, metadata_directory=None):
    return _wheel(wheel_directory, {NAME + ".pth": os.path.abspath("src") + "\\n"})
'''


def write_project(directory, name, version):
    os.makedirs(os.path.join(directory, "src", name), exist_ok=True)
    with open(os.path.join(directory, "src", name, "__init__.py"), "w") as f:
        f.write("VERSION = %r\n" % version)
    with open(os.path.join(directory, "NAMEVER"), "w") as f:
        f.write("%s %s\n" % (name, version))
    with open(os.path.join(directory, "backend.py"), "w") as f:
        f.write(BACKEND)
    with open(os.path.join(directory, "pyproject.toml"), "w") as f:
        f.write('[build-system]\nrequires = []\nbuild-backend = "backend"\nbackend-path = ["."]\n')


def main():
    base = sys.argv[1]
    wheels = os.path.join(base, "wheels")
    os.makedirs(wheels, exist_ok=True)
    for name, version in WHEELS:
        write_wheel(wheels, name, version, {name.lower() + "/__init__.py": "VERSION = %r\n" % version})
    write_project(os.path.join(base, "proj"), "goldproj", "0.1")
    write_project(os.path.join(base, "repo"), "goldvcs", "0.2")
    with open(os.path.join(base, "requirements.txt"), "w") as f:
        f.write("goldpkg==1.5\nGold-Dash\n")


main()
