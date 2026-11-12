#!/usr/bin/env python3
"""Run the real CLI against a private, disposable scripted instance and Dolt child."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

root = Path(__file__).resolve().parent.parent
binary = root / 'bin' / 'fae'
dolt = os.environ.get('FAE_TEST_DOLT_BINARY')
if not dolt or not Path(dolt).is_file():
    sys.exit('Set FAE_TEST_DOLT_BINARY to an installed Dolt executable')
base = '/private/tmp' if sys.platform == 'darwin' else '/var/tmp'
with tempfile.TemporaryDirectory(prefix='fae-cli-smoke-', dir=base) as scratch:
    home = Path(scratch) / 'instances'
    log_path = Path(scratch) / 'daemon.log'
    common = ['--home', str(home), '--instance', 'smoke', '--json']
    def call(*args):
        result = subprocess.run([str(binary), *common, *args], cwd=root,
                                capture_output=True, text=True, timeout=15)
        if result.returncode:
            raise RuntimeError(f'{args}: exit {result.returncode}: {result.stderr}')
        return json.loads(result.stdout)
    def launch(log):
        process = subprocess.Popen([str(binary), *common, 'serve', '--workspace', str(root),
                                    '--adapter', 'scripted', '--dolt', dolt, '--foreground'],
                                   cwd=root, stdout=log, stderr=log)
        for _ in range(150):
            if process.poll() is not None:
                raise RuntimeError('daemon exited: ' + log_path.read_text())
            try:
                call('status')
                return process
            except RuntimeError:
                time.sleep(.1)
        process.terminate()
        process.wait(timeout=15)
        raise RuntimeError('daemon readiness timed out: ' + log_path.read_text())
    process = None
    try:
        with log_path.open('w+') as log:
            process = launch(log)
            # A plain-text message authorizes only the first class it is used for;
            # the run (task) and the later shutdowns (runtime) need both classes.
            call('message', '--id', 'authorization', '--text', json.dumps({
                'text': 'Run local scripted smoke work and shut down this disposable instance.',
                'authorize': {'classes': ['task', 'runtime']}}))
            admitted = call('run', '--id', 'smoke-task', '--prompt', 'Scripted smoke rehearsal',
                            '--authorizing-message', 'authorization')
            assert admitted['version'] == 1, admitted
            for _ in range(400):
                snapshot = call('history', '--task', 'smoke-task')
                if snapshot['state'] == 'Done':
                    break
                if snapshot['state'] == 'Escalated':
                    raise RuntimeError(json.dumps(snapshot))
                time.sleep(.1)
            else:
                raise RuntimeError('task did not finish')
            board = call('board', '--limit', '1')
            assert [item['id'] for item in board['items']] == ['smoke-task'], board
            shown = call('show', '--task', 'smoke-task')
            assert shown['tasks'][0]['state'] == 'Done', shown
            assert [w['workflow_id'] for w in call('template', 'workflow-list')] == ['default']
            cursor, seen, refs = 0, [], []
            while True:
                page = call('history', '--task', 'smoke-task', '--summary',
                            '--after', str(cursor), '--limit', '7')
                seen.extend(event['sequence'] for event in page['events'])
                refs.extend(page['artifact_references'])
                cursor = page.get('next_cursor', 0)
                if not cursor:
                    break
            assert seen == sorted(set(seen)) and len(seen) > 7, seen
            assert len(refs) == 22, refs
            before = snapshot['version']
            provenance = snapshot['payload']['job']['provenance']
            assert len(provenance) >= 20, len(provenance)
            replay = call('run', '--id', 'smoke-task', '--prompt', 'Scripted smoke rehearsal',
                          '--authorizing-message', 'authorization')
            assert replay['version'] == 1, replay
            events = call('events', '--limit', '1000')
            assert events and events[-1]['sequence'] > events[0]['sequence'], events
            call('shutdown', '--authorizing-message', 'authorization')
            assert process.wait(timeout=20) == 0, log_path.read_text()
            process = launch(log)
            restored = call('history', '--task', 'smoke-task')
            assert restored['state'] == 'Done' and restored['version'] == before, restored
            call('shutdown', '--authorizing-message', 'authorization')
            assert process.wait(timeout=20) == 0, log_path.read_text()
            print(json.dumps({'cli_smoke': 'passed', 'artifact_records': len(provenance),
                              'events': len(events), 'resume_preserved_version': before}))
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
