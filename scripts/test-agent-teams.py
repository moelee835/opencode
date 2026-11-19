#!/usr/bin/env python3
"""Deterministic end-to-end test against a local OpenAI-compatible fake provider.
Usage: python3 scripts/test-agent-teams.py /absolute/path/to/opencode
No external API calls or model credentials are used by the configured agents.
"""
import http.server
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import threading
import time

binary = str(Path(sys.argv[1]).resolve())
requests = []
class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps({'data': [{'id': name, 'object': 'model', 'type': 'llm', 'loaded_context_length': 8192} for name in ['team-test', 'team-test-alt']]}).encode())
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        requests.append(body)
        if not body.get('stream'):
            data = {'id': 'title', 'object': 'chat.completion', 'model': 'team-test', 'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'Team test'}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}}
            self.send_response(200); self.send_header('Content-Type', 'application/json'); self.end_headers()
            self.wfile.write(json.dumps(data).encode()); return
        last = body['messages'][-1]
        text = json.dumps(last.get('content', ''))
        markers = re.findall(r'MAKE_MARKER:([a-z]+)', text)
        marker = markers[-1] if markers else None
        # Lead reports are summarized, never echoed back to workers.
        write = marker and last['role'] == 'user' and 'Team role: lead' not in text
        delta = {'role': 'assistant', 'content': 'Task complete; results available.'}
        reason = 'stop'
        if write:
            delta = {'role': 'assistant', 'tool_calls': [{'index': 0, 'id': 'call-' + marker, 'type': 'function', 'function': {'name': 'write', 'arguments': json.dumps({'file_path': str(workspace / (marker + '.txt')), 'content': 'verified ' + marker})}}]}
            reason = 'tool_calls'
        self.send_response(200); self.send_header('Content-Type', 'text/event-stream'); self.end_headers()
        for d, finish in [(delta, None), ({}, reason)]:
            chunk = {'id': 'test-completion', 'object': 'chat.completion.chunk', 'created': 1, 'model': 'team-test', 'choices': [{'index': 0, 'delta': d, 'finish_reason': finish}]}
            self.wfile.write(('data: ' + json.dumps(chunk) + '\n\n').encode()); self.wfile.flush()
        self.wfile.write(b'data: [DONE]\n\n')

server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Provider)
threading.Thread(target=server.serve_forever, daemon=True).start()
with tempfile.TemporaryDirectory(prefix='opencode-team-e2e-') as directory:
    workspace = Path(directory)
    env = dict(os.environ, GORACE='atexit_sleep_ms=0', LOCAL_ENDPOINT=f'http://127.0.0.1:{server.server_port}/v1')
    config = {'agents': {name: {'model': 'local.team-test'} for name in ['coder', 'task', 'title', 'summarizer']}, 'lsp': {}, 'mcpServers': {}, 'autoCompact': False}
    (workspace / '.opencode.json').write_text(json.dumps(config))
    def run(*args):
        p = subprocess.run([binary, *args], cwd=workspace, env=env, capture_output=True, text=True, timeout=15)
        assert p.returncode == 0, p.stderr + p.stdout
        return json.loads(p.stdout)
    def wait(predicate, label):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if predicate(): return
            time.sleep(.1)
        raise AssertionError(label + ': ' + json.dumps(run('teams', 'status', team['id'])))
    team = run('teams', 'create', 'integration')
    a = run('teams', 'spawn', team['id'], 'alpha', '--prompt', 'MAKE_MARKER:alpha', '--model', 'local.team-test-alt')
    b = run('teams', 'spawn', team['id'], 'beta', '--prompt', 'MAKE_MARKER:beta')
    initial = run('teams', 'status', team['id'])['tasks']
    gated = run('teams', 'task', team['id'], '--title', 'Approved followup', '--prompt', 'MAKE_MARKER:approved', '--assignee', a['session_id'], '--depends-on', ','.join(t['id'] for t in initial), '--require-approval')
    log = open(workspace / 'runtime.log', 'w+')
    runtime = subprocess.Popen([binary, 'teams', 'run', team['id'], '--allow-tools'], cwd=workspace, env=env, stdout=log, stderr=log)
    try:
        wait(lambda: (workspace / 'alpha.txt').exists() and (workspace / 'beta.txt').exists(), 'workers did not execute write tools')
        assert not (workspace / 'approved.txt').exists(), 'approval gate bypassed'
        run('teams', 'approve', team['id'], gated['id'])
        wait(lambda: (workspace / 'approved.txt').exists(), 'dependent approved task not executed')
        run('messages', 'send', '--from', team['lead'], '--to', b['session_id'], '--body', 'MAKE_MARKER:message', '--key', 'e2e-message')
        wait(lambda: (workspace / 'message.txt').exists(), 'message did not wake worker')
        wait(lambda: any(r['session_id'] == team['lead'] and r['status'] == 'completed' for r in run('teams', 'status', team['id'])['runs']), 'lead did not synthesize results')
        wait(lambda: all(t['status'] == 'completed' for t in run('teams', 'status', team['id'])['tasks']), 'tasks did not complete')
        run('teams', 'stop', team['id'])
        assert runtime.wait(timeout=10) == 0
        print('PASS: two teammates, independent conversations/models, real write tools, dependency and approval gate, automatic message wake-up, lead results, graceful stop')
        assert len([r for r in requests if r.get('stream')]) >= 8
        assert {'team-test', 'team-test-alt'} <= {r['model'] for r in requests}
    except BaseException:
        log.flush(); log.seek(0); print(log.read(), file=sys.stderr); raise
    finally:
        if runtime.poll() is None:
            runtime.send_signal(signal.SIGINT)
            try: runtime.wait(timeout=10)
            except subprocess.TimeoutExpired: runtime.kill(); runtime.wait()
        log.close()
server.shutdown()
