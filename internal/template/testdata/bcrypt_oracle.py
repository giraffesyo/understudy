"""Generates bcrypt.json for TestBcryptMatchesPasslib: what ansible-core's
password_hash filter returns, through passlib, for the 'blowfish' (bcrypt)
hashtype: the hash, or the error ansible-core reports (its message, and
the message of the exception it was raised from). Test-data tooling only;
run it with the Python ansible-core and passlib are installed in, on a
controller without libxcrypt (macOS), where ansible-core hashes through
passlib.

Usage: python3 bcrypt_oracle.py > bcrypt.json
"""
import json, warnings

warnings.simplefilter("ignore")

from ansible.plugins.filter.core import get_encrypted_password

PASSWORDS = ["pw", "", "mypassword", "s3cr3t!", "ünïcødé", "x" * 71, "x" * 72, "x" * 73, "a\x00b"]
SALTS = [None, "abcdefghijklmnopqrstuv", "abcdefghijklmnopqrstuu", "ABCDEFGHIJKLMNOPQRSTU.", "./0123456789ABCDEFGHIJ",
         "abcdefghijklmnopqrstu9", "abc", "abcdefghijklmnopqrstuvw", "abcdefghijklmnopqrstu!", "!bcdefghijklmnopqrstuv", ""]
ROUNDS = [None, 4, 5, 10, 3, 32, "5", 0]
IDENTS = [None, "2b", "2a", "2y", "2", "2x", "zz", ""]


def case(password, salt, rounds, ident, salt_size=None):
    out = {"password": password, "salt": salt, "rounds": rounds, "ident": ident, "salt_size": salt_size}
    try:
        out["hash"] = get_encrypted_password(password, "blowfish", salt=salt, rounds=rounds, ident=ident, salt_size=salt_size)
    except Exception as ex:
        out["error"] = str(ex)
        cause = ex.__cause__
        if cause is not None:
            out["cause"] = str(cause)
    return out


cases = []
for salt in SALTS:
    if salt is None:
        continue  # random
    for password in PASSWORDS:
        cases.append(case(password, salt, 4, None))
for rounds in ROUNDS:
    cases.append(case("pw", "abcdefghijklmnopqrstuv", rounds, None))
for ident in IDENTS:
    for password in ["pw", "x" * 30, ""]:
        cases.append(case(password, "abcdefghijklmnopqrstuv", 4, ident))
for salt_size in [22, 10, 30]:
    cases.append(case("pw", "abcdefghijklmnopqrstuv", 4, None, salt_size))
cases.append(case("pw", "abcdefghijklmnopqrstuv", 12, None))

print(json.dumps(cases, indent=1, ensure_ascii=False))
