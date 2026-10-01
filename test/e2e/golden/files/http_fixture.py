"""A deterministic HTTP server for the golden corpus (uri, get_url, ...).

    python3 http_fixture.py <dir>

Listens on an ephemeral TCP port on 127.0.0.1 (written to <dir>/http.port)
and on a unix socket in a fresh /tmp directory (its path written to
<dir>/http.sockpath: a socket path must stay short). It exits on GET
/shutdown or after 120 idle seconds, so a failed playbook never leaves it
behind for long.

With <dir>/cert.pem and <dir>/key.pem present it also serves HTTPS on a
second ephemeral port (<dir>/https.port).

A minimal passive-mode FTP server (<dir>/ftp.port) serves /pub/file.txt
and a /pub directory listing to user anonymous; any other user fails to
log in.

Responses carry fixed Server and Date headers so result dicts compare
byte-for-byte across runs. The request echo (/echo) reports only the
headers a test sets on purpose, never the port-bearing Host.
"""

import base64
import gzip
import hashlib
import json
import os
import socket
import socketserver
import ssl
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

LAST_REQUEST = [time.time()]
ECHO_HEADERS = ('content-type', 'content-length', 'authorization', 'cache-control',
                'if-modified-since', 'cookie', 'x-test', 'x-secret', 'user-agent')


class Handler(BaseHTTPRequestHandler):
    server_version = 'fixture'
    sys_version = ''

    def version_string(self):
        return 'fixture'

    def date_time_string(self, timestamp=None):
        return 'Thu, 01 Jan 2026 00:00:00 GMT'

    def address_string(self):
        return 'client'

    def log_message(self, *args):
        pass

    def _body(self):
        n = int(self.headers.get('Content-Length') or 0)
        return self.rfile.read(n) if n else b''

    def _send(self, code, body=b'', ctype='text/plain', headers=(), length=True):
        self.send_response(code)
        if ctype:
            self.send_header('Content-Type', ctype)
        for k, v in headers:
            self.send_header(k, v)
        if length:
            self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(body)

    def _json(self, code, obj, headers=()):
        self._send(code, json.dumps(obj, sort_keys=True).encode(), 'application/json', headers)

    def handle_any(self):
        LAST_REQUEST[0] = time.time()
        target = self.path
        if target.startswith('http://'):
            # A proxied request: the absolute URI's path is what counts.
            target = '/' + target.split('/', 3)[3] if target.count('/') >= 3 else '/'
            self.proxied = True
        path, _, query = target.partition('?')
        body = self._body()
        if path == '/shutdown':
            self._send(200, b'bye\n')
            threading.Thread(target=shutdown, daemon=True).start()
        elif path == '/json':
            self._json(200, {'name': 'fixture', 'items': [1, 2, 3], 'nested': {'ok': True}})
        elif path == '/ordered-json':
            self._send(200, b'{"zeta": 1, "alpha": {"y": 2.0, "b": [{"k2": 1, "k1": null}]}, "mid": 1.5}', 'application/json')
        elif path == '/vnd-json':
            self._send(200, b'{"kind": "vnd"}', 'application/vnd.api+json')
        elif path == '/bad-json':
            self._send(200, b'{not json', 'application/json')
        elif path == '/text':
            self._send(200, b'hello from the fixture\n', 'text/plain; charset=utf-8')
        elif path == '/latin1':
            self._send(200, 'café\n'.encode('latin-1'), 'text/plain; charset=iso-8859-1')
        elif path == '/nolength':
            self._send(200, b'streamed body\n', 'text/plain', length=False)
            self.close_connection = True
        elif path == '/notype':
            self._send(200, b'no content type\n', None)
        elif path == '/echo':
            hdrs = {k.lower(): v for k, v in self.headers.items() if k.lower() in ECHO_HEADERS}
            if 'content-type' in hdrs and 'boundary=' in hdrs['content-type']:
                hdrs['content-type'] = hdrs['content-type'].split('boundary=')[0] + 'boundary=B'
            text = body.decode('utf-8', 'replace')
            ctype = self.headers.get('Content-Type', '')
            if 'boundary=' in ctype:
                text = text.replace(ctype.split('boundary=')[1].strip('"'), 'B')
            self._json(200, {'method': self.command, 'query': query, 'headers': hdrs, 'body': text})
        elif path.startswith('/status/'):
            code = int(path.split('/')[2])
            self._send(code, ('status %d\n' % code).encode(), 'text/plain', [('X-Status', str(code))])
        elif path.startswith('/redirect/'):
            code = int(path.split('/')[2])
            target = '/echo' if query == 'echo' else '/json'
            self._send(code, b'moved\n', 'text/plain', [('Location', target)])
        elif path == '/redirect-abs':
            self._send(302, b'', 'text/plain', [('Location', 'http://localhost/text')])
        elif path == '/cookies':
            self._send(200, b'cookies set\n', 'text/plain',
                       [('Set-Cookie', 'session=abc123; Path=/'),
                        ('Set-Cookie', 'theme=dark; Path=/'),
                        ('X-Multi', 'one'), ('X-Multi', 'two')])
        elif path == '/gzip':
            data = gzip.compress(b'this was gzipped\n', mtime=0)
            self._send(200, data, 'text/plain', [('Content-Encoding', 'gzip')])
        elif path == '/basic':
            want = 'Basic ' + base64.b64encode(b'alice:s3cret').decode()
            if self.headers.get('Authorization') == want:
                self._json(200, {'user': 'alice'})
            else:
                self._send(401, b'auth required\n', 'text/plain',
                           [('WWW-Authenticate', 'Basic realm="fixture"')])
        elif path.startswith('/download/'):
            name = path.split('/', 2)[2]
            hdrs = []
            if query == 'disposition':
                hdrs.append(('Content-Disposition', 'attachment; filename="from-header.txt"'))
            if self.headers.get('If-Modified-Since') and query == 'cache':
                self._send(304, b'', None)
                return
            self._send(200, ('payload of %s\n' % name).encode(), 'text/plain', hdrs)
        elif path == '/slow':
            time.sleep(3)
            self._send(200, b'late\n')
        elif path == '/digest':
            auth = self.headers.get('Authorization', '')
            if auth.startswith('Digest ') and self._digest_ok(auth[7:]):
                self._json(200, {'user': 'alice', 'auth': 'digest'})
            else:
                self._send(401, b'digest required\n', 'text/plain',
                           [('WWW-Authenticate', 'Digest realm="fixture", nonce="n0nce", qop="auth", '
                                                 'algorithm="MD5", opaque="0paque"')])
        elif path == '/bearer':
            self._send(401, b'token required\n', 'text/plain', [('WWW-Authenticate', 'Bearer realm="fixture"')])
        elif path == '/loop':
            self._send(302, b'', 'text/plain', [('Location', '/loop')])
        elif path == '/bad-scheme':
            self._send(302, b'', 'text/plain', [('Location', 'gopher://localhost/x')])
        elif path == '/proxied':
            self._json(200, {'proxied': getattr(self, 'proxied', False), 'path': self.path.split('?')[0]
                             if not self.path.startswith('http') else self.path})
        elif path == '/xml':
            self._send(200, b'<a>b</a>\n', 'application/xml')
        else:
            self._send(404, b'not found\n', 'text/plain')

    def _digest_ok(self, header):
        params = {}
        for part in header.split(','):
            k, _, v = part.strip().partition('=')
            params[k] = v.strip('"')
        h = lambda x: hashlib.md5(x.encode()).hexdigest()
        ha1 = h('alice:fixture:s3cret')
        ha2 = h('%s:%s' % (self.command, params.get('uri', '')))
        want = h(':'.join([ha1, params.get('nonce', ''), params.get('nc', ''), params.get('cnonce', ''),
                           params.get('qop', ''), ha2]))
        return params.get('response') == want and params.get('opaque') == '0paque'

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = do_OPTIONS = handle_any

    def do_TRACE(self):
        self.handle_any()


class UnixServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        req, _ = super().get_request()
        return req, ('client', 0)


class TCPServer(socketserver.ThreadingMixIn, HTTPServer):
    daemon_threads = True


class FTPHandler(socketserver.StreamRequestHandler):
    """Just enough of RFC 959 for urllib's FTPHandler."""

    FILES = {'/pub/file.txt': b'ftp content\n'}
    DIRS = {'/', '/pub'}

    def reply(self, line):
        self.wfile.write(line.encode() + b'\r\n')

    def handle(self):
        LAST_REQUEST[0] = time.time()
        cwd, user, pasv = '/', None, None
        self.reply('220 fixture FTP ready')
        for raw in self.rfile:
            LAST_REQUEST[0] = time.time()
            cmd, _, arg = raw.decode().rstrip('\r\n').partition(' ')
            cmd = cmd.upper()
            if cmd == 'USER':
                user = arg
                self.reply('331 Password required')
            elif cmd == 'PASS':
                if user == 'anonymous':
                    self.reply('230 Logged in')
                else:
                    self.reply('530 Login incorrect.')
            elif cmd == 'CWD':
                new = arg if arg.startswith('/') else os.path.normpath(os.path.join(cwd, arg))
                if new in self.DIRS:
                    cwd = new
                    self.reply('250 Directory successfully changed.')
                else:
                    self.reply('550 Failed to change directory.')
            elif cmd == 'PWD':
                self.reply('257 "%s" is the current directory' % cwd)
            elif cmd == 'TYPE':
                self.reply('200 Switching to %s mode.' % ('Binary' if arg == 'I' else 'ASCII'))
            elif cmd == 'PASV':
                pasv = socket.socket()
                pasv.bind(('127.0.0.1', 0))
                pasv.listen(1)
                port = pasv.getsockname()[1]
                self.reply('227 Entering Passive Mode (127,0,0,1,%d,%d).' % (port >> 8, port & 255))
            elif cmd in ('RETR', 'LIST'):
                path = os.path.normpath(os.path.join(cwd, arg)) if arg else cwd
                if cmd == 'RETR':
                    data = self.FILES.get(path)
                elif path in self.DIRS:
                    data = ''.join('-rw-r--r-- 1 ftp ftp %d Jan 01 2026 %s\r\n' % (len(v), os.path.basename(k))
                                   for k, v in sorted(self.FILES.items()) if os.path.dirname(k) == path).encode()
                else:
                    data = None
                if data is None:
                    self.reply('550 Failed to open file.')
                    continue
                conn, _ = pasv.accept()
                if cmd == 'RETR':
                    self.reply('150 Opening BINARY mode data connection for %s (%d bytes).' % (arg, len(data)))
                else:
                    self.reply('150 Here comes the directory listing.')
                conn.sendall(data)
                conn.close()
                pasv.close()
                self.reply('226 Transfer complete.')
            elif cmd == 'QUIT':
                self.reply('221 Goodbye.')
                return
            else:
                self.reply('502 Command not implemented.')


class FTPServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    daemon_threads = True
    allow_reuse_address = True


SERVERS = []


SOCKET = []


def shutdown():
    time.sleep(0.2)
    for s in SOCKET:
        try:
            os.unlink(s)
            os.rmdir(os.path.dirname(s))
        except OSError:
            pass
    os._exit(0)


def main():
    base = os.path.abspath(sys.argv[1])
    sock = os.path.join(tempfile.mkdtemp(prefix='fx', dir='/tmp'), 's')
    with open(os.path.join(base, 'http.sockpath'), 'w') as f:
        f.write(sock)
    SOCKET.append(sock)
    unix = UnixServer(sock, Handler)
    tcp = TCPServer(('127.0.0.1', 0), Handler)
    servers = [unix, tcp]
    cert, key = os.path.join(base, 'cert.pem'), os.path.join(base, 'key.pem')
    if os.path.exists(cert):
        tls = TCPServer(('127.0.0.1', 0), Handler)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(cert, key)
        tls.socket = ctx.wrap_socket(tls.socket, server_side=True)
        with open(os.path.join(base, 'https.port'), 'w') as f:
            f.write(str(tls.server_address[1]))
        servers.append(tls)
    ftp = FTPServer(('127.0.0.1', 0), FTPHandler)
    with open(os.path.join(base, 'ftp.port'), 'w') as f:
        f.write(str(ftp.server_address[1]))
    servers.append(ftp)
    for srv in servers:
        SERVERS.append(srv)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
    with open(os.path.join(base, 'http.port.tmp'), 'w') as f:
        f.write(str(tcp.server_address[1]))
    os.rename(os.path.join(base, 'http.port.tmp'), os.path.join(base, 'http.port'))
    while time.time() - LAST_REQUEST[0] < 120:
        time.sleep(1)
    shutdown()


if __name__ == '__main__':
    main()
