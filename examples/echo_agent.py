#!/usr/bin/env python3
"""Local webhook example: authenticate, deduplicate and reply via wbot send."""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import time


def secret(value):
    if value.startswith('env:'):
        name = value[4:]
        if not os.environ.get(name):
            raise SystemExit(f'Missing environment variable: {name}')
        return os.environ[name]
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--wbot', default='wbot')
    parser.add_argument('--config', type=Path, default=Path.home() / '.config/wbot/config.json')
    parser.add_argument('--port', type=int, default=8787)
    args = parser.parse_args()
    config = args.config.expanduser().resolve()
    if not config.is_file():
        raise SystemExit('Create the wbot configuration with wbot init first.')
    binary = shutil.which(args.wbot) or str(Path(args.wbot).resolve())
    if not Path(binary).is_file():
        raise SystemExit('Provide the compiled executable with --wbot PATH.')
    settings = json.loads(config.read_text())
    hook = settings['webhook']
    token = secret(hook.get('token', ''))
    key = secret(hook.get('signing_key', ''))
    database = config.with_name(config.stem + '.echo.sqlite3')
    with sqlite3.connect(database) as conn:
        conn.execute('CREATE TABLE IF NOT EXISTS handled (id TEXT PRIMARY KEY)')
    database.chmod(0o600)

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def finish_response(self, status):
            self.send_response(status)
            self.send_header('Content-Length', '0')
            self.end_headers()

        def do_POST(self):
            if self.path != '/events':
                return self.finish_response(404)
            if token and not hmac.compare_digest(self.headers.get('Authorization', ''), 'Bearer ' + token):
                return self.finish_response(401)
            try:
                size = int(self.headers.get('Content-Length', '0'))
                if size <= 0 or size > 1024 * 1024:
                    return self.finish_response(413)
                body = self.rfile.read(size)
                if key:
                    stamp = self.headers.get('X-Wbot-Timestamp', '')
                    if abs(time.time() - int(stamp)) > 300:
                        return self.finish_response(401)
                    expected = 'sha256=' + hmac.new(key.encode(), stamp.encode() + b'.' + body, hashlib.sha256).hexdigest()
                    if not hmac.compare_digest(expected, self.headers.get('X-Wbot-Signature', '')):
                        return self.finish_response(401)
                event = json.loads(body)
                identity, sender = event['id'], event['sender_id']
                if not isinstance(identity, str) or not isinstance(sender, str) or not identity or not sender:
                    return self.finish_response(400)
                if not event.get('can_reply'):
                    return self.finish_response(204)
                with sqlite3.connect(database, timeout=10) as conn:
                    conn.execute('BEGIN IMMEDIATE')
                    if conn.execute('SELECT 1 FROM handled WHERE id = ?', (identity,)).fetchone():
                        return self.finish_response(204)
                    text = str(event.get('text') or '附件已收到。')[:1000]
                    command = [binary, '--config', str(config), 'send', '--to', sender, '--text', '已收到：' + text]
                    result = subprocess.run(command, capture_output=True, text=True, timeout=30)
                    if result.returncode:
                        return self.finish_response(502)
                    conn.execute('INSERT INTO handled (id) VALUES (?)', (identity,))
                print('Replied to event', identity, flush=True)
                return self.finish_response(204)
            except (ValueError, KeyError, TypeError):
                return self.finish_response(400)
            except (sqlite3.Error, OSError, subprocess.TimeoutExpired):
                return self.finish_response(503)

    server = ThreadingHTTPServer(('127.0.0.1', args.port), Handler)
    print(f'Echo Agent listening on http://127.0.0.1:{args.port}/events', flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == '__main__':
    main()
