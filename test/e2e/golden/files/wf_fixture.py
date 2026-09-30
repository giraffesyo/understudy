"""A TCP banner server for the wait_for golden corpus.

    python3 wf_fixture.py <dir>

Listens on an ephemeral port on 127.0.0.1 (written to <dir>/wf.port),
greets every connection with an SSH-style banner and closes it. It exits
when a client sends "quit", or after 30 idle seconds, so a failed playbook
never leaves it behind for long.
"""

import os
import socket
import sys

BANNER = b"SSH-2.0-golden_9.1 build=42\r\n"

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 0))
srv.listen(16)
srv.settimeout(30)
tmp = os.path.join(sys.argv[1], "wf.port.tmp")
with open(tmp, "w") as f:
    f.write(str(srv.getsockname()[1]))
os.rename(tmp, os.path.join(sys.argv[1], "wf.port"))

while True:
    try:
        conn, _ = srv.accept()
    except socket.timeout:
        break
    try:
        conn.settimeout(0.2)
        conn.sendall(BANNER)
        try:
            if conn.recv(16).startswith(b"quit"):
                conn.close()
                break
        except (socket.timeout, OSError):
            pass
    finally:
        conn.close()
srv.close()
