#!/usr/bin/env python3
"""Exercise a compiled wbot through local HTTP simulators. No WeChat login needed."""
import contextlib
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time


def run(binary):
    version = subprocess.check_output([str(binary), 'version'], text=True, timeout=5).strip().split()[1]
    failures = []
    complete = threading.Event()
    replies = []
    incoming_sent = threading.Event()
    config_path = None

    class WeChat(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            if self.headers.get('Authorization') != 'Bearer mock-bot-token':
                failures.append('Missing WeChat authentication')
            if body.get('base_info', {}).get('bot_agent') != 'wbot/' + version:
                failures.append('Missing wbot application identity')
            result = {'ret': 0}
            if self.path.endswith('/getupdates'):
                result.update(get_updates_buf='cursor-2', msgs=[])
                if not incoming_sent.is_set():
                    incoming_sent.set()
                    result['msgs'] = [{
                        'message_id': 9007199254740993, 'message_type': 1,
                        'message_state': 2, 'from_user_id': 'mock-owner',
                        'to_user_id': 'mock-bot', 'context_token': 'mock-context-token',
                        'item_list': [{'type': 1, 'text_item': {'text': '你好，wbot'}}],
                    }]
                else:
                    time.sleep(0.05)
            elif self.path.endswith('/sendmessage'):
                message = body['msg']
                if message['context_token'] != 'mock-context-token' or message['to_user_id'] != 'mock-owner':
                    failures.append('Wrong reply context or recipient')
                replies.append(message['item_list'][0]['text_item']['text'])
            elif '/msg/notify' not in self.path:
                failures.append('Unexpected WeChat endpoint ' + self.path)
            data = json.dumps(result).encode()
            self.send_response(200)
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            with contextlib.suppress(BrokenPipeError):
                self.wfile.write(data)

    class Agent(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            raw = self.rfile.read(int(self.headers['Content-Length']))
            event = json.loads(raw)
            if event['message_id'] != '9007199254740993':
                failures.append('Message ID lost precision')
            if b'mock-context-token' in raw or b'mock-bot-token' in raw:
                failures.append('WeChat credentials leaked into webhook')
            stamp = self.headers.get('X-Wbot-Timestamp', '')
            expected = 'sha256=' + hmac.new(b'mock-signing-key', stamp.encode() + b'.' + raw, hashlib.sha256).hexdigest()
            if self.headers.get('X-Wbot-Signature') != expected:
                failures.append('Webhook signature mismatch')
            if self.headers.get('Authorization') != 'Bearer mock-hook-token':
                failures.append('Webhook authentication mismatch')
            reply = subprocess.run([
                str(binary), '--config', str(config_path), 'send',
                '--to', event['sender_id'], '--text', '收到：' + event['text'],
            ], capture_output=True, text=True, timeout=10)
            if reply.returncode:
                failures.append('wbot send failed: ' + reply.stderr)
            self.send_response(204)
            self.send_header('Content-Length', '0')
            self.end_headers()
            complete.set()

    wx = ThreadingHTTPServer(('127.0.0.1', 0), WeChat)
    agent = ThreadingHTTPServer(('127.0.0.1', 0), Agent)
    for server in (wx, agent):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix='wbot-smoke-') as folder:
            root = Path(folder)
            config_path = root / 'config.json'
            state = root / 'state'
            state.mkdir(mode=0o700)
            with socket.socket() as port:
                port.bind(('127.0.0.1', 0))
                listen = '127.0.0.1:' + str(port.getsockname()[1])
            config_path.write_text(json.dumps({
                'listen': listen, 'local_token': 'mock-local-token-12345678901234567890',
                'state_dir': str(state), 'allow_users': [],
                'webhook': {'url': f'http://127.0.0.1:{agent.server_port}/events',
                            'token': 'mock-hook-token', 'signing_key': 'mock-signing-key'},
            }))
            config_path.chmod(0o600)
            (state / 'session.json').write_text(json.dumps({
                'credentials': {'token': 'mock-bot-token', 'bot': 'mock-bot',
                                'owner': 'mock-owner', 'endpoint': f'http://127.0.0.1:{wx.server_port}'},
                'reply_tokens': {}, 'pending': [], 'delivered': [], 'cursor': '',
            }))
            process = subprocess.Popen([str(binary), '--config', str(config_path), 'run'],
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                if not complete.wait(12):
                    raise AssertionError('WeChat → wbot → Agent → wbot send → WeChat did not finish')
                deadline = time.monotonic() + 3
                while time.monotonic() < deadline:
                    saved = json.loads((state / 'session.json').read_text())
                    if not saved['pending'] and saved['delivered']:
                        break
                    time.sleep(0.02)
                else:
                    raise AssertionError('Delivery acknowledgement was not persisted')
                status = subprocess.run([str(binary), '--config', str(config_path), 'status'],
                                        capture_output=True, text=True, check=True, timeout=5)
                observed = json.loads(status.stdout)
                if observed['pending_messages'] != 0:
                    failures.append('Queue did not drain')
                if observed['activity']['delivery_attempts'] != 1:
                    failures.append('Unexpected Agent delivery attempts')
                health = subprocess.run([str(binary), '--config', str(config_path), 'health'],
                                        capture_output=True, text=True, check=True, timeout=5)
                if not json.loads(health.stdout)['healthy']:
                    failures.append('Health check failed')
                if replies != ['收到：你好，wbot']:
                    failures.append('Wrong or duplicate reply: ' + repr(replies))
                process.send_signal(signal.SIGTERM)
                out, err = process.communicate(timeout=5)
                if process.returncode != 0:
                    failures.append('Daemon shutdown failed: ' + err)
                if failures:
                    raise AssertionError('\n'.join(failures))
                print(json.dumps({'result': 'passed', 'webhook_events': 1, 'wechat_replies': len(replies),
                                  'pending_messages': 0, 'health_check': True,
                                  'graceful_shutdown': True}, indent=2))
            finally:
                if process.poll() is None:
                    process.kill()
                    process.communicate(timeout=5)
    finally:
        for server in (wx, agent):
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    if len(sys.argv) != 2:
        raise SystemExit('Usage: python3 tests/smoke.py /path/to/wbot')
    run(Path(sys.argv[1]).resolve())
