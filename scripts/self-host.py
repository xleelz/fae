#!/usr/bin/env python3
"""Run Fae on Fae: a live instance whose workspace is a clone of this repository.

  setup       clone the source at a commit, write the live config and workflow
  start       serve the instance in the background, authorize it, install the workflow
  submit      admit a task (title, prompt, declared files) against that workflow
  status      instance status and each task's state, stage, attempts and test runs
  stop        drain active turns, then shut the instance down
  land        list the commits on the clone's fae/integration past the base;
              --fetch-into REPO fetches them into REPO as branch fae-self/<date>
  acceptance  setup, start and submit one task file, wait, write evidence JSON

Standard library only; run with python3 -I. `--self-test` runs the unit tests of
the pure parts without binaries or a provider. See docs/SELF_HOSTING.md.
Credentials are never read, copied or linked: --claude-config-dir is passed to
the bridge only as CLAUDE_CONFIG_DIR in the live config's environment.
"""
import argparse
import datetime
import json
import os
import pathlib
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = pathlib.Path(__file__).resolve().parent.parent
INSTANCE = 'self'
DEFAULT_HOME = '~/fae-self/i'
DEFAULT_DIR = '~/fae-self/repo'
DEFAULT_MODEL = 'claude-opus-5-5'
PROVIDER = 'claude'
# Under --home, beside the instance directory. A leading dot is not a valid
# instance name, so it can never collide with one.
STATE_DIR = '.self-host'
INTEGRATION_BRANCH = 'fae/integration'
INTEGRATION_REF = 'refs/heads/' + INTEGRATION_BRANCH
GIT_NAME = 'Fae Self-Host'
GIT_EMAIL = 'fae-self-host@localhost'
# Engine test runs get TMPDIR <home>/self/worktrees/command-tmp-NNNNNNNNN
# (effects/broker.go), and Fae's tests bind Unix sockets under t.TempDir()
# there. A socket path is limited to 108 bytes, so the worktree root must stay
# short; the coordinator's measurement of the default suite sets this bound.
WORKTREES_MAX_BYTES = 40
# The dogfood's working values (scripts/dogfood.py); the live timeout is the
# bridge's provider timeout plus a margin, 900 s at the default.
HEAVY_CAPACITY = 2
STALL_SECONDS = 300
STALL_SAFE_REPLAY = True
DEFAULT_PROVIDER_TIMEOUT = 840
SESSION_MARGIN = 60
# Fae's suite spawns nested git and go calls and prints about 5.6 MB under -x.
NESTED_COMMANDS = 'account'
MAX_OUTPUT_BYTES = 16 * 1024 * 1024
TERMINAL_STATES = ('Done', 'Escalated', 'Retired', 'Cancelled')
DEFERRED = ('IOPressure', 'IOPressureHold', 'HostPressure', 'HostPressureHold', 'PaneLimit', 'MemoryReservation')
EXIT_UNREACHABLE = 3


class SelfHostError(Exception):
    """A refusal the operator can act on; printed without a traceback."""


class CLIError(SelfHostError):
    def __init__(self, args, code, stderr):
        super().__init__('fae %s: exit %d: %s' % (' '.join(args[:2]), code, stderr.strip()))
        self.code, self.stderr = code, stderr


class Unreachable(CLIError):
    pass


def log(message):
    print(utc_now() + ' ' + message, file=sys.stderr, flush=True)


def utc_now():
    return datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')


def parse_time(value):
    """Parse a Go RFC 3339 time; Go may print nine fractional digits."""
    m = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)', value or '')
    if not m:
        raise ValueError('not an RFC 3339 time: %r' % value)
    fraction = (m.group(2) or '0')[:6].ljust(6, '0')
    zone = '+00:00' if m.group(3) == 'Z' else m.group(3)
    return datetime.datetime.fromisoformat('%s.%s%s' % (m.group(1), fraction, zone))


def seconds_between(start, end):
    if not start or not end:
        return None
    return round((parse_time(end) - parse_time(start)).total_seconds(), 3)


# ---------------------------------------------------------------------------
# Pure parts: paths, configuration, workflow, evidence. Unit tested below.

def canonical_path(raw):
    """An absolute, canonical spelling (no '.', '..' or repeated slashes)."""
    return os.path.normpath(os.path.abspath(os.path.expanduser(raw)))


def check_home(home):
    """Refuse a --home that would break Unix socket paths in engine test runs."""
    if not os.path.isabs(home) or os.path.normpath(home) != home or home.startswith('//'):
        raise SelfHostError('--home must be an absolute canonical path, got %r' % home)
    worktrees = os.path.join(home, INSTANCE, 'worktrees')
    size = len(os.fsencode(worktrees))
    if size > WORKTREES_MAX_BYTES:
        raise SelfHostError(
            '--home %s is too long: %s is %d bytes, over %d. Engine test runs use TMPDIR '
            '%s/command-tmp-NNNNNNNNN, Fae\'s own tests bind Unix sockets under t.TempDir() '
            'inside it, and a socket path is limited to 108 bytes. Choose a --home of at most '
            '%d bytes.' % (home, worktrees, size, WORKTREES_MAX_BYTES, worktrees,
                           WORKTREES_MAX_BYTES - len('/%s/worktrees' % INSTANCE)))
    return worktrees


def host_command_vectors(workflow):
    """Every distinct HostCommand argv of a workflow document, sorted."""
    vectors = {tuple(stage['command']) for stage in workflow['stages'].values()
               if stage.get('primitive') == 'HostCommand'}
    return [list(v) for v in sorted(vectors)]


def narrow_routes(workflow, provider, model):
    """scripts/dogfood.py's per-instance route chain (FAE-WF-082).

    The bridge serves one provider: keep only that provider's bindings when the
    role already starts with the requested binding, else use the requested one.
    Nothing else in the document changes; the input is not modified.
    """
    narrowed = json.loads(json.dumps(workflow))
    for role in narrowed['roles'].values():
        first = role['routes'][0]
        if (first['provider'], first['model']) == (provider, model):
            role['routes'] = [r for r in role['routes'] if r['provider'] == provider]
        else:
            role['routes'] = [{'provider': provider, 'model': model, 'effort': 'medium'}]
    return narrowed


def live_config(*, base, bridge, claude, claude_config_dir, provider_timeout, workflow, host_ledger, herdr=None):
    """The instance's live config with no task scope pinned (derived scope).

    files is empty and there is no test_file, test_id, expected_failure, tasks
    or commit_message, so each task's scope, RED identity and commit message come
    from the task itself. The environment holds only CLAUDE_CONFIG_DIR.
    """
    for name, path in (('bridge', bridge), ('claude', claude), ('claude config dir', claude_config_dir), ('host ledger', host_ledger)) + ((('herdr', herdr),) if herdr else ()):
        if not path or not os.path.isabs(path):
            raise SelfHostError('%s must be an absolute path, got %r' % (name, path))
    if not re.fullmatch(r'[0-9a-f]{40}|[0-9a-f]{64}', base or ''):
        raise SelfHostError('base must be a full commit SHA, got %r' % base)
    if not isinstance(provider_timeout, int) or provider_timeout < 1:
        raise SelfHostError('--timeout-seconds must be a positive integer')
    config = {
        'adapter': 'multiplexer' if herdr else 'direct',
        'command': [bridge, '--claude', claude, '--config', '{config}', '--timeout', '%ds' % provider_timeout],
        'environment': {'CLAUDE_CONFIG_DIR': claude_config_dir},
        'allowed_commands': host_command_vectors(workflow),
        'files': [],
        'base': base,
        'integration_ref': INTEGRATION_REF,
        'commit_message': '',
        'host_ledger': host_ledger,
        'heavy_capacity': HEAVY_CAPACITY,
        'timeout_seconds': provider_timeout + SESSION_MARGIN,
        'stall_seconds': STALL_SECONDS,
        'stall_safe_replay': STALL_SAFE_REPLAY,
        'nested_commands': NESTED_COMMANDS,
        'max_output_bytes': MAX_OUTPUT_BYTES,
    }
    if herdr:
        config['herdr_binary'] = herdr
    return config


def clean_declared(paths):
    """Declared files as clean relative paths, sorted and deduplicated."""
    out = set()
    for raw in paths:
        path = raw.strip()
        if not path or path.startswith('/') or '\\' in path or '\0' in path:
            raise SelfHostError('declared file must be a relative path: %r' % raw)
        parts = path.split('/')
        if any(p in ('', '.', '..') for p in parts) or path.startswith('.git/') or path == '.git':
            raise SelfHostError('declared file must be a clean path inside the repository: %r' % raw)
        out.add(path)
    return sorted(out)


def declared_warnings(declared):
    tests = [p for p in declared if p.endswith('_test.go')]
    if not declared:
        return ['no declared files: the spec turn must declare the scope before any change phase']
    if len(tests) != 1:
        return ['declared files hold %d *_test.go files; the RED turn needs exactly one test file in scope' % len(tests)]
    return []


def deferred_refusal(stderr):
    """A host-admission deferral precedes any write; retry it as dogfood does."""
    text = stderr + '\n'
    return any('host admission: ' + kind + '\n' in text for kind in DEFERRED) or 'retry control after current run' in stderr


def merge_pages(pages, keys):
    merged = {key: [] for key in keys}
    for page in pages:
        for key in keys:
            merged[key].extend(page.get(key) or [])
    return merged


def drain_settled(snapshot):
    """Draining with no worker running.

    `drained` also waits for every in-flight dispatch, but a dispatch parked as
    uncertain stays in flight until the operator resolves it (it survives a
    restart unchanged), so it must not hold the stop forever.
    """
    return bool(snapshot.get('drained') or (snapshot.get('draining') and snapshot.get('active') == 0))


def task_row(show, snapshot=None):
    """One task's operator line fields from `show` and, if it fit, legacy history."""
    job = ((snapshot or {}).get('payload') or {}).get('job') or {}
    row = {'id': show['id'], 'state': show['state'], 'stage': show.get('stage', ''), 'parked': bool(show.get('parked')),
           'in_flight': bool(show.get('in_flight')), 'reason': show.get('reason', ''),
           'attempts': job.get('attempts'), 'test_runs': job.get('test_runs'), 'failures': job.get('failures') or {}}
    title = (((snapshot or {}).get('payload') or {}).get('admission') or {}).get('title')
    if title:
        row['title'] = title
    if show['state'] == 'Escalated' or job.get('escalation_kind'):
        row['escalation'] = {'kind': job.get('escalation_kind', ''), 'detail': job.get('escalation_detail', ''),
                             'reason': show.get('escalation') or job.get('reason', '')}
    return row


def format_task_row(row):
    unknown = lambda v: '?' if v is None else v
    line = '%s  %s  stage=%s  attempts=%s  test_runs=%s' % (row['id'], row['state'], row['stage'], unknown(row['attempts']), unknown(row['test_runs']))
    if row.get('in_flight'):
        line += '  in-flight'
    if row.get('parked'):
        line += '  parked(%s)' % row.get('reason', '')
    if row.get('title'):
        line += '\n  title: ' + row['title']
    if row.get('escalation'):
        e = row['escalation']
        line += '\n  escalation: %s %s %s' % (e['kind'], e['detail'], e['reason'])
    return line


def history_summary(show, records, snapshot=None):
    job = ((snapshot or {}).get('payload') or {}).get('job') or {}
    transitions = [{'at': t.get('created_at'), 'from': t.get('from'), 'to': t.get('to'), 'attempt': t.get('attempt'), 'note': t.get('note')}
                   for t in records.get('transitions', [])]
    artifacts = [{'at': a.get('produced_at'), 'stage': a.get('stage'), 'kind': a.get('kind'), 'role': a.get('role'),
                  'provider': a.get('provider', ''), 'model': a.get('model', ''), 'dispatch_id': a.get('dispatch_id')}
                 for a in records.get('artifacts', [])]
    counts = {}
    for a in artifacts:
        counts[a['kind']] = counts.get(a['kind'], 0) + 1
    done = next((t['at'] for t in transitions if t['to'] == 'Done'), None)
    admitted = transitions[0]['at'] if transitions else None
    summary = {
        'state': show.get('state'), 'stage': show.get('stage'),
        'attempts': job.get('attempts', max((t['attempt'] or 0 for t in transitions), default=0)),
        'test_runs': job.get('test_runs', counts.get('EngineRun', 0)),
        'failures': job.get('failures') or {},
        'admitted_at': admitted, 'done_at': done, 'admission_to_done_seconds': seconds_between(admitted, done),
        'transitions': transitions, 'artifact_counts': counts, 'artifacts': artifacts,
    }
    if show.get('state') == 'Escalated' or job.get('escalation_kind'):
        summary['escalation'] = {'kind': job.get('escalation_kind', ''), 'detail': job.get('escalation_detail', ''),
                                 'reason': show.get('escalation') or show.get('reason') or job.get('reason', '')}
    return summary


def event_payload(event):
    """An event's payload when it is an object; some daemon events carry a
    bare value (for example `true`), which names no stage or session."""
    payload = event.get('payload')
    return payload if isinstance(payload, dict) else {}


def engine_runs(events, artifacts, workflow):
    """Each HostCommand dispatch with its wall time and recorded result.

    Wall time runs from the session-launched event to the session-reaped event
    of the dispatch (daemon commit times), so it includes any host-lease wait.
    """
    stages = workflow['stages']
    host = {name for name, stage in stages.items() if stage.get('primitive') == 'HostCommand'}
    launched, reaped = {}, {}
    for event in events:
        payload = event_payload(event)
        if payload.get('stage') not in host:
            continue
        if event.get('kind') == 'session-launched':
            launched.setdefault(payload.get('session'), event)
        elif event.get('kind') == 'session-reaped':
            reaped[payload.get('session')] = event
    results = {a.get('dispatch_id'): a for a in artifacts if a.get('kind') in ('EngineRun', 'Format')}
    runs = []
    for session, event in sorted(launched.items(), key=lambda item: item[1].get('sequence', 0)):
        payload, end = event_payload(event), reaped.get(session)
        artifact = results.get(payload.get('dispatch_id'))
        runs.append({'stage': payload['stage'], 'dispatch_id': payload.get('dispatch_id'), 'attempt': payload.get('attempt'),
                     'command': stages[payload['stage']].get('command'),
                     'launched_at': event.get('timestamp'), 'reaped_at': end.get('timestamp') if end else None,
                     'wall_seconds': seconds_between(event.get('timestamp'), end.get('timestamp')) if end else None,
                     'artifact': artifact.get('kind') if artifact else None,
                     'result': artifact.get('output') if artifact else None})
    return runs


def is_turn_start(e):
    return e.get('type') == 'turn.started' or (e.get('type') == 'system' and e.get('subtype') == 'init')


def is_turn_done(e):
    return e.get('type') == 'turn.completed' or (e.get('type') == 'result' and e.get('subtype') == 'success' and not e.get('is_error'))


def provider_turns(sessions, events):
    """Provider turns from the session logs, ordered as the daemon launched them."""
    stage, order = {}, {}
    for event in events:
        payload = event_payload(event)
        if event.get('kind') == 'session-launched' and payload.get('session'):
            stage.setdefault(payload['session'], payload.get('stage', ''))
            order.setdefault(payload['session'], event.get('sequence', 0))
    turns = []
    for record in sessions:
        launch, records = record['launch'], record['events']
        binding = launch.get('binding') or {}
        sid = launch.get('session_id', '')
        turn = {'session_id': sid, 'dispatch_id': launch.get('dispatch_id', ''), 'role': launch.get('role', ''),
                'stage': stage.get(sid, ''), 'provider': binding.get('provider', ''), 'model': binding.get('model', ''),
                'started': any(is_turn_start(e) for e in records), 'completed': any(is_turn_done(e) for e in records)}
        result = next((e for e in reversed(records) if e.get('type') == 'result'), None)
        if result:
            for key in ('subtype', 'is_error', 'duration_ms', 'duration_api_ms', 'num_turns', 'total_cost_usd'):
                if key in result:
                    turn[key] = result[key]
        turns.append(turn)
    turns.sort(key=lambda t: (order.get(t['session_id'], float('inf')), t['session_id']))
    return turns


def parse_commits(log_text):
    """`git log --format=%H%x09%s` lines as commit records."""
    commits = []
    for line in log_text.splitlines():
        if line.strip():
            sha, _, subject = line.partition('\t')
            commits.append({'sha': sha, 'subject': subject})
    return commits


def landed_summary(base, tip, log_text, stats, diff_stat):
    commits = parse_commits(log_text)
    for commit in commits:
        commit['stat'] = stats.get(commit['sha'], '').strip()
    return {'integration_ref': INTEGRATION_REF, 'base': base, 'tip': tip, 'moved': tip != base,
            'landed_commit': tip if tip != base else None, 'commits': commits, 'diff_stat': diff_stat.strip()}


def final_status(status, show):
    if status is None:
        return {'instance': None, 'task': show}
    snapshot = status.get('snapshot') or {}
    return {'instance': {'pid': status.get('pid'), 'adapter': status.get('adapter'), 'uptime_seconds': status.get('uptime_seconds'),
                         'version': status.get('version'), 'default_workflow': status.get('default_workflow'),
                         'counts': snapshot.get('counts'), 'drive_alive': (snapshot.get('drive') or {}).get('alive')},
            'task': show}


def assemble_evidence(*, task, outcome, show, records, events, snapshot, workflow, sessions, provider_log, landed, status, timing):
    """The acceptance evidence document, built only from recorded CLI and git output."""
    turns = provider_turns(sessions, events)
    return {
        'schema': 'fae-self-host-acceptance/1',
        'outcome': outcome,
        'task': task,
        'timing': timing,
        'history': history_summary(show or {}, records, snapshot),
        'landed': landed,
        'engine_runs': engine_runs(events, records.get('artifacts', []), workflow),
        'provider_turns': turns,
        'provider_turn_counts': {'launched': len(turns), 'completed': sum(t['completed'] for t in turns)},
        'provider_log': [row for row in provider_log if str(row.get('session', '')).startswith(task['id'] + '/')],
        'final_status': final_status(status, show),
    }


# ---------------------------------------------------------------------------
# Effects: files, git and the fae CLI.

def state_path(home):
    return pathlib.Path(home) / STATE_DIR / 'state.json'


def write_private(path, text):
    """Write an owner-only (0600) file atomically."""
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix='.' + path.name + '.')
    try:
        with os.fdopen(fd, 'w') as out:
            out.write(text)
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise


def load_state(home):
    path = state_path(home)
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        raise SelfHostError('no self-host instance at %s; run setup first' % home) from None


def save_state(state):
    write_private(state_path(state['home']), json.dumps(state, indent=2) + '\n')


def git(*args, cwd=None):
    r = subprocess.run(['git', *args], cwd=cwd, text=True, capture_output=True)
    if r.returncode:
        raise SelfHostError('git %s: %s' % (' '.join(args), r.stderr.strip()))
    return r.stdout.strip()


def git_version():
    out = git('--version')
    m = re.search(r'(\d+)\.(\d+)', out)
    return (int(m.group(1)), int(m.group(2))) if m else (0, 0)


def executable(path, name):
    if not path:
        raise SelfHostError('%s not found; pass its path' % name)
    path = canonical_path(path)
    if not os.path.isfile(path) or not os.access(path, os.X_OK):
        raise SelfHostError('%s %s is not an executable file' % (name, path))
    return path


def inside_repository(path):
    p = pathlib.Path(path)
    for parent in [p, *p.parents]:
        if (parent / '.git').exists():
            return str(parent)
    return None


def physical_path(path):
    """The path with its existing part's symlinks resolved."""
    p = pathlib.Path(path)
    missing = []
    while not p.exists() and p != p.parent:
        missing.insert(0, p.name)
        p = p.parent
    return os.path.join(os.path.realpath(p), *missing)


def fae(state, *args, timeout=120, retries=20, wait=15):
    """Call the instance through the fae CLI, retrying host-admission deferrals."""
    cmd = [state['fae'], '--home', state['home'], '--instance', INSTANCE, '--json', *args]
    for attempt in range(retries + 1):
        try:
            r = subprocess.run(cmd, cwd=state['clone'], text=True, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            raise CLIError(args, -1, 'no reply within %ds' % timeout) from None
        if r.returncode and attempt < retries and deferred_refusal(r.stderr):
            log('deferred by host admission, retrying in %ds: %s' % (wait, r.stderr.strip()))
            time.sleep(wait)
            continue
        if r.returncode == EXIT_UNREACHABLE:
            raise Unreachable(args, r.returncode, r.stderr)
        if r.returncode:
            raise CLIError(args, r.returncode, r.stderr)
        return json.loads(r.stdout) if r.stdout.strip() else None


def running(state):
    try:
        return fae(state, 'status', retries=0)
    except Unreachable:
        return None


def authorization(state, classes, text, fresh=False):
    """An operator message authorizing classes; reuse a recent one that covers them.

    The daemon refuses messages older than its --message-max-age (24 h by
    default), so a message is reused for at most 23 hours.
    """
    auth = state.get('authorization')
    if auth and not fresh and set(classes) <= set(auth['classes']):
        age = (datetime.datetime.now(datetime.timezone.utc) - parse_time(auth['sent_at'])).total_seconds()
        if age < 23 * 3600:
            return auth['id']
    message = 'self-host-%s-%s' % (datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'), secrets.token_hex(3))
    fae(state, 'message', '--id', message, '--text', json.dumps({'text': text, 'authorize': {'classes': sorted(classes)}}))
    state['authorization'] = {'id': message, 'classes': sorted(classes), 'sent_at': utc_now()}
    save_state(state)
    return message


# ---------------------------------------------------------------------------
# Subcommands.

def setup(a):
    home = canonical_path(a.home)
    check_home(home)
    repo = inside_repository(home)
    if repo:
        raise SelfHostError('--home %s is inside the repository %s; the instance home must be outside repositories' % (home, repo))
    if physical_path(home) != home:
        raise SelfHostError('--home %s passes through a symlink (physical path %s); fae requires a physical home path' % (home, physical_path(home)))
    if os.path.lexists(os.path.join(home, INSTANCE)) or state_path(home).exists():
        raise SelfHostError('%s already holds a self-host instance; stop it and remove %s, or choose another --home' % (home, home))
    clone = canonical_path(a.dir)
    if os.path.lexists(clone) and (not os.path.isdir(clone) or os.listdir(clone)):
        raise SelfHostError('--dir %s exists and is not empty; the clone must be fresh' % clone)
    if git_version() < (2, 45):
        raise SelfHostError('git 2.45 or newer is required (reconciliation passes trees to git merge-tree)')
    binary = executable(a.fae or str(ROOT / 'bin' / 'fae'), 'fae')
    bridge = executable(a.bridge or str(ROOT / 'bin' / 'fae-claude-bridge'), 'bridge')
    claude = executable(a.claude or shutil.which('claude'), 'claude')
    dolt = executable(a.dolt, 'dolt')
    herdr = executable(a.herdr, 'herdr-core') if a.herdr else None
    config_dir = canonical_path(a.claude_config_dir)
    if not os.path.isdir(config_dir):
        raise SelfHostError('--claude-config-dir %s is not a directory' % config_dir)
    host_ledger = canonical_path(a.host_ledger)
    source = canonical_path(a.source)
    base = git('-C', source, 'rev-parse', '--verify', '--end-of-options', a.commit + '^{commit}')
    if git('-C', source, 'status', '--porcelain'):
        log('note: %s has uncommitted changes; the clone holds only commit %s' % (source, base))
    exported = subprocess.run([binary, 'workflow', 'default', '--local', '--json'], text=True, capture_output=True)
    if exported.returncode:
        raise SelfHostError('fae workflow default --local --json: %s' % exported.stderr.strip())
    shipped = json.loads(exported.stdout)
    workflow = narrow_routes(shipped, PROVIDER, a.model)
    config = live_config(base=base, bridge=bridge, claude=claude, claude_config_dir=config_dir, provider_timeout=a.timeout_seconds,
                         workflow=shipped, host_ledger=host_ledger, herdr=herdr)
    if host_command_vectors(workflow) != config['allowed_commands']:
        raise SelfHostError('route narrowing changed the workflow\'s host commands')

    git('clone', '--quiet', '--no-local', source, clone)
    try:
        if subprocess.run(['git', '-C', clone, 'cat-file', '-e', base + '^{commit}'], capture_output=True).returncode:
            raise SelfHostError('commit %s is not reachable from a branch of %s; the clone cannot check it out' % (base, source))
        git('-C', clone, 'checkout', '--quiet', '--detach', base)
        git('-C', clone, 'config', 'user.name', GIT_NAME)
        git('-C', clone, 'config', 'user.email', GIT_EMAIL)
        # The clone keeps one branch, the un-checked-out integration target,
        # and no remote: nothing in it may reach the source repository.
        for branch in git('-C', clone, 'for-each-ref', '--format=%(refname:short)', 'refs/heads/').split():
            git('-C', clone, 'branch', '--quiet', '-D', branch)
        git('-C', clone, 'branch', INTEGRATION_BRANCH, base)
        git('-C', clone, 'remote', 'remove', 'origin')
    except SelfHostError:
        shutil.rmtree(clone)  # the clone this call made, so a rerun starts fresh
        raise

    files = pathlib.Path(home) / STATE_DIR
    pathlib.Path(home).mkdir(mode=0o700, parents=True, exist_ok=True)
    write_private(files / 'live.json', json.dumps(config, indent=2) + '\n')
    write_private(files / 'workflow.json', json.dumps(workflow, indent=2) + '\n')
    state = {'schema': 1, 'home': home, 'clone': clone, 'source': source, 'commit': a.commit, 'base': base,
             'fae': binary, 'bridge': bridge, 'claude': claude, 'dolt': dolt, 'herdr': herdr, 'adapter': config['adapter'],
             'model': a.model, 'live_config': str(files / 'live.json'), 'workflow_file': str(files / 'workflow.json'),
             'workflow': None, 'authorization': None, 'created_at': utc_now()}
    save_state(state)
    print('clone        %s (detached at %s; branch %s; no remote)' % (clone, base, INTEGRATION_BRANCH))
    print('home         %s (instance %s; serve log %s)' % (home, INSTANCE, os.path.join(home, INSTANCE + '.serve.log')))
    print('live config  %s' % state['live_config'])
    print('workflow     %s (shipped default, Local projection, routes narrowed to %s/%s)' % (state['workflow_file'], PROVIDER, a.model))
    print('state        %s' % state_path(home))
    print(json.dumps(config, indent=2))
    return state


def preflight_path():
    missing = [tool for tool in ('go', 'gofmt', 'git') if not shutil.which(tool)]
    if missing:
        raise SelfHostError('%s not on PATH; the engine runs the workflow\'s host commands with the daemon\'s PATH' % ', '.join(missing))


def start(a):
    home = canonical_path(a.home)
    state = load_state(home)
    preflight_path()
    status = running(state)
    if status:
        log('instance already running (pid %s)' % status.get('pid'))
    else:
        cmd = [state['fae'], '--home', home, '--instance', INSTANCE, '--json', 'serve', '--workspace', state['clone'],
               '--adapter', state['adapter'], '--live-config', state['live_config'], '--dolt', state['dolt'],
               '--prophet-provider', PROVIDER, '--prophet-model', state['model']]
        # Without --foreground, serve detaches once ready and logs to HOME/INSTANCE.serve.log.
        r = subprocess.run(cmd, cwd=state['clone'], text=True, capture_output=True, timeout=600)
        if r.returncode:
            raise SelfHostError('fae serve failed (exit %d): %s\nlog: %s' % (r.returncode, r.stderr.strip(), os.path.join(home, INSTANCE + '.serve.log')))
        print(r.stdout.strip())
        deadline = time.monotonic() + 60
        while not (status := running(state)):
            if time.monotonic() > deadline:
                raise SelfHostError('instance did not answer status; see %s' % os.path.join(home, INSTANCE + '.serve.log'))
            time.sleep(0.5)
    # As dogfood does: one operator message authorizing task and runtime
    # control; installing the workflow (WorkflowInstall) also needs governance.
    install = state.get('workflow') is None
    classes = ['task', 'runtime'] + (['governance'] if install else [])
    text = ('Operate the Fae self-host instance on the clone %s: admit and run tasks with the narrowed default workflow, '
            'and drain or stop this instance.%s' % (state['clone'], ' Install that workflow revision.' if install else ''))
    message = authorization(state, classes, text, fresh=True)
    if install:
        put = fae(state, 'template', 'workflow-put', '--content', state['workflow_file'], '--authorizing-message', message)
        state['workflow'] = {'id': put['WorkflowID'], 'revision': put['Number'], 'hash': put['Hash']}
        save_state(state)
        print('installed workflow %s revision %d (%s)' % (put['WorkflowID'], put['Number'], put['Hash']))
    else:
        installed = state['workflow']
        rows = fae(state, 'template', 'workflow-list')
        match = [r for r in rows if r['workflow_id'] == installed['id'] and r['revision'] == installed['revision']]
        if not match or match[0]['hash'] != installed['hash'] or match[0]['retired']:
            raise SelfHostError('installed workflow %s revision %d is missing, changed or retired' % (installed['id'], installed['revision']))
        print('workflow %s revision %d already installed' % (installed['id'], installed['revision']))
    print('instance %s running (pid %s); authorizing message %s' % (INSTANCE, status.get('pid'), message))
    return state


def submit_task(state, *, title, prompt=None, file=None, declared=(), task_id=None):
    """Admit one task against the installed workflow revision."""
    if (prompt is None) == (file is None):
        raise SelfHostError('give exactly one of --prompt or --file')
    declared = clean_declared(declared)
    for warning in declared_warnings(declared):
        log('warning: ' + warning)
    if not state.get('workflow'):
        raise SelfHostError('no workflow installed; run start first')
    message = authorization(state, ['task'], 'Admit and run self-host tasks on %s.' % state['clone'])
    args = ['run', '--title', title, '--workflow', state['workflow']['id'], '--revision', str(state['workflow']['revision']),
            '--authorizing-message', message]
    args += ['--prompt', prompt] if prompt is not None else ['--file', canonical_path(file)]
    if task_id:
        args += ['--id', task_id]
    for path in declared:
        args += ['--declared-file', path]
    admitted = fae(state, *args)
    print('admitted %s (%s) on %s@%s' % (admitted['id'], admitted['state'], admitted['workflow_id'], admitted['workflow_revision']))
    return admitted


def submit(a):
    state = load_state(canonical_path(a.home))
    submit_task(state, title=a.title, prompt=a.prompt, file=a.file, declared=a.declared_file or [], task_id=a.id)
    return 0


def legacy_history(state, task):
    """The full checkpoint, or None when it exceeds the read page bound."""
    try:
        return fae(state, 'history', '--task', task, retries=0)
    except Unreachable:
        raise
    except CLIError:
        return None


def show_task(state, task):
    return fae(state, 'show', '--task', task, retries=0)['tasks'][0]


def status(a):
    state = load_state(canonical_path(a.home))
    try:
        st = fae(state, 'status', '--tasks', retries=0)
    except Unreachable:
        print('instance %s at %s is not running' % (INSTANCE, state['home']))
        return 1
    rows = [task_row(show_task(state, t['id']), legacy_history(state, t['id'])) for t in st['snapshot'].get('tasks') or []]
    if a.json:
        print(json.dumps({'instance': final_status(st, None)['instance'], 'workflow': state.get('workflow'), 'tasks': rows}, indent=2))
        return 0
    snap = st['snapshot']
    counts = ', '.join('%s %d' % kv for kv in sorted((snap.get('counts') or {}).items())) or 'none'
    print('instance %s pid %s adapter %s up %.0fs drive %s; tasks: %s' % (
        INSTANCE, st.get('pid'), st.get('adapter'), st.get('uptime_seconds', 0),
        'alive' if (snap.get('drive') or {}).get('alive') else 'stopped', counts))
    if state.get('workflow'):
        print('workflow %s@%d; base %s; clone %s' % (state['workflow']['id'], state['workflow']['revision'], state['base'], state['clone']))
    for row in rows:
        print(format_task_row(row))
    return 0


def wait_unreachable(state, seconds):
    deadline = time.monotonic() + seconds
    while running(state):
        if time.monotonic() > deadline:
            raise SelfHostError('instance still answers after %ds; see %s' % (seconds, os.path.join(state['home'], INSTANCE + '.serve.log')))
        time.sleep(1)


def stop(a):
    state = load_state(canonical_path(a.home))
    if not running(state):
        print('instance %s is not running' % INSTANCE)
        return 0
    message = authorization(state, ['runtime'], 'Drain and stop the self-host instance on %s.' % state['clone'])
    if not a.now:
        # Let active turns finish so nothing is left uncertain at restart.
        fae(state, 'drain', '--authorizing-message', message)
        deadline = time.monotonic() + a.drain_wait
        while not drain_settled(snapshot := fae(state, 'status', retries=0)['snapshot']):
            if time.monotonic() > deadline:
                raise SelfHostError('not drained after %ds; rerun stop, or stop --now to interrupt active turns' % a.drain_wait)
            time.sleep(2)
        if not snapshot.get('drained'):
            log('note: parked in-flight dispatches remain; after the next start, see status and resolve them with fae resolve-uncertain')
    fae(state, 'shutdown', '--authorizing-message', message)
    wait_unreachable(state, 300)
    print('instance %s stopped' % INSTANCE)
    return 0


def integration_landed(state):
    clone, base = state['clone'], state['base']
    tip = git('-C', clone, 'rev-parse', '--verify', INTEGRATION_REF)
    log_text = git('-C', clone, 'log', '--reverse', '--format=%H%x09%s', base + '..' + tip)
    stats = {c['sha']: git('-C', clone, 'show', '--stat', '--format=', c['sha']) for c in parse_commits(log_text)}
    return landed_summary(base, tip, log_text, stats, git('-C', clone, 'diff', '--stat', base, tip))


def land(a):
    state = load_state(canonical_path(a.home))
    landed = integration_landed(state)
    if not landed['commits']:
        print('nothing landed: %s is at the base %s' % (INTEGRATION_BRANCH, state['base']))
        return 0
    for commit in landed['commits']:
        print('%s %s' % (commit['sha'], commit['subject']))
        if commit['stat']:
            print('  ' + commit['stat'].replace('\n', '\n  '))
    if a.fetch_into:
        target = canonical_path(a.fetch_into)
        branch = 'fae-self/' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%d')
        # A plain fetch: no merge, no push, and no forced update of an existing branch.
        git('-C', target, 'fetch', '--no-tags', state['clone'], '%s:refs/heads/%s' % (INTEGRATION_REF, branch))
        print('fetched %d commit(s) into %s as %s (%s); review, test and merge them yourself' % (len(landed['commits']), target, branch, landed['tip']))
    return 0


def read_sessions(home, task_id):
    """Each provider log of the task's sessions: launch fields and turn records."""
    keep = ('system', 'result', 'turn.started', 'turn.completed', 'thread.started')
    out = []
    for path in sorted((pathlib.Path(home) / INSTANCE / 'sessions').glob('session-*/provider-*/events.jsonl')):
        try:
            launch = json.loads((path.parent.parent / 'config.json').read_text())
        except (OSError, ValueError):
            continue
        if launch.get('task_id') != task_id:
            continue
        records = []
        for line in path.read_text(errors='replace').splitlines():
            try:
                record = json.loads(line)
            except ValueError:
                continue
            if isinstance(record, dict) and record.get('type') in keep:
                records.append(record)
        out.append({'launch': {k: launch.get(k) for k in ('session_id', 'dispatch_id', 'task_id', 'role', 'binding', 'workflow_id')}, 'events': records})
    return out


def paged(state, base_args, keys, cursor_key='next_cursor'):
    pages, after = [], 0
    while True:
        page = fae(state, *base_args, '--after', str(after), '--limit', '500', retries=0)
        pages.append(page)
        after = page.get(cursor_key) or 0
        if not after:
            return merge_pages(pages, keys)


def provider_log(state):
    rows, after = [], 0
    while True:
        page = fae(state, 'provider-log', '--after', str(after), '--limit', '500', retries=0) or []
        rows.extend(page)
        if len(page) < 500:
            return rows
        after = page[-1]['sequence']


def collect_evidence(state, task, outcome, timing):
    show = show_task(state, task['id'])
    records = paged(state, ['history', '--task', task['id'], '--records'], ('transitions', 'artifacts', 'decisions'))
    events = paged(state, ['history', '--task', task['id'], '--summary'], ('events',))['events']
    workflow = json.loads(pathlib.Path(state['workflow_file']).read_text())
    return assemble_evidence(task=task, outcome=outcome, show=show, records=records, events=events,
                             snapshot=legacy_history(state, task['id']), workflow=workflow,
                             sessions=read_sessions(state['home'], task['id']), provider_log=provider_log(state),
                             landed=integration_landed(state), status=running(state), timing=timing)


def load_task_file(path):
    try:
        task = json.loads(pathlib.Path(path).read_text())
    except (OSError, ValueError) as e:
        raise SelfHostError('--task %s: %s' % (path, e)) from None
    if not isinstance(task, dict) or set(task) - {'title', 'prompt', 'declared_files'} or not isinstance(task.get('title'), str) \
            or not isinstance(task.get('prompt'), str) or not isinstance(task.get('declared_files', []), list):
        raise SelfHostError('--task %s must be a JSON object with string title and prompt and a declared_files list' % path)
    return {'title': task['title'], 'prompt': task['prompt'], 'declared_files': clean_declared(task.get('declared_files', []))}


def acceptance(a):
    task = load_task_file(a.task)
    home = canonical_path(a.home)
    evidence_path = pathlib.Path(canonical_path(a.evidence)) if a.evidence else pathlib.Path(home) / STATE_DIR / (
        'acceptance-%s.json' % datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'))
    setup(a)
    started = utc_now()
    outcome, admitted, evidence = 'error', None, None
    try:
        state = start(a)
        admitted = submit_task(state, title=task['title'], prompt=task['prompt'], declared=task['declared_files'])
        task = dict(task, id=admitted['id'])
        deadline, last = time.monotonic() + a.wait, None
        while True:
            try:
                shown = show_task(state, task['id'])
            except Unreachable:
                raise SelfHostError('the instance stopped answering; see %s' % os.path.join(home, INSTANCE + '.serve.log')) from None
            except CLIError as e:  # a busy daemon; the next poll asks again
                log('poll failed: %s' % e)
                time.sleep(a.poll)
                continue
            seen = (shown['state'], shown.get('stage'), shown.get('parked'), shown.get('reason'))
            if seen != last:
                log('%s %s stage=%s%s' % (task['id'], shown['state'], shown.get('stage'),
                                          ' parked(%s)' % shown.get('reason', '') if shown.get('parked') else ''))
                last = seen
            if shown['state'] in TERMINAL_STATES:
                outcome = shown['state']
                break
            if time.monotonic() > deadline:
                outcome = 'timeout'
                break
            time.sleep(a.poll)
    except KeyboardInterrupt:
        outcome = 'interrupted'
        raise
    except SelfHostError as e:
        outcome = 'error: %s' % e
        raise
    finally:
        if admitted:
            try:
                state = load_state(home)
                evidence = collect_evidence(state, task, outcome, {'started_at': started, 'finished_at': utc_now(), 'wait_seconds': a.wait})
                write_private(evidence_path, json.dumps(evidence, indent=2) + '\n')
                log('evidence written to %s' % evidence_path)
            except Exception as e:  # keep the original failure visible
                log('evidence not collected: %s' % e)
        if not a.keep_running:
            try:
                a.now = outcome not in TERMINAL_STATES
                stop(a)
            except Exception as e:
                log('stop failed: %s' % e)
    print(json.dumps({'outcome': outcome, 'task': task.get('id'), 'evidence': str(evidence_path) if evidence else None}))
    return 0 if outcome == 'Done' else 1


# ---------------------------------------------------------------------------
# Command line.

def parser():
    home = argparse.ArgumentParser(add_help=False)
    home.add_argument('--home', default=DEFAULT_HOME, help='the Fae --home of the self-host instance (default %(default)s)')
    setup_flags = argparse.ArgumentParser(add_help=False)
    setup_flags.add_argument('--source', default=str(ROOT), help='repository to clone (default this repository)')
    setup_flags.add_argument('--commit', default='HEAD', help='commit to clone at (default HEAD)')
    setup_flags.add_argument('--dir', default=DEFAULT_DIR, help='where the clone lives (default %(default)s)')
    setup_flags.add_argument('--fae', help='fae binary (default bin/fae)')
    setup_flags.add_argument('--bridge', help='Claude bridge binary (default bin/fae-claude-bridge)')
    setup_flags.add_argument('--claude', help='Claude Code executable (default claude on PATH)')
    setup_flags.add_argument('--claude-config-dir', required=True, help='passed to the bridge only as CLAUDE_CONFIG_DIR; never read or copied')
    setup_flags.add_argument('--dolt', required=True, help='Dolt executable (fae serve needs it)')
    setup_flags.add_argument('--herdr', help='herdr-core executable; selects the multiplexer adapter (default the direct adapter)')
    setup_flags.add_argument('--host-ledger', default='~/.fae/host-ledger', help='shared host lease ledger (default %(default)s, the FAE-HOST-031 default)')
    setup_flags.add_argument('--model', default=DEFAULT_MODEL, help='Claude model for every role and Prophet (default %(default)s)')
    setup_flags.add_argument('--timeout-seconds', type=int, default=DEFAULT_PROVIDER_TIMEOUT,
                             help='bridge provider timeout; the live session timeout is this plus %d s (default %%(default)s)' % SESSION_MARGIN)
    p = argparse.ArgumentParser(description=__doc__.split('\n\n')[0], formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest='command', required=True)
    sub.add_parser('setup', parents=[home, setup_flags], help='clone and write the live config and workflow')
    sub.add_parser('start', parents=[home], help='serve the instance in the background and install the workflow')
    s = sub.add_parser('submit', parents=[home], help='admit a task')
    s.add_argument('--title', required=True)
    s.add_argument('--prompt', help='task text, stating its acceptance')
    s.add_argument('--file', help='task text file (markdown)')
    s.add_argument('--declared-file', action='append', help='a file the task may read and change; repeat it (include one *_test.go)')
    s.add_argument('--id', help='task ID (default generated by fae)')
    s = sub.add_parser('status', parents=[home], help='instance and task status')
    s.add_argument('--json', action='store_true')
    s = sub.add_parser('stop', parents=[home], help='drain, then shut down')
    s.add_argument('--now', action='store_true', help='skip draining; active turns are interrupted')
    s.add_argument('--drain-wait', type=int, default=1200, help='seconds to wait for active turns (default %(default)s)')
    s = sub.add_parser('land', parents=[home], help='list landed commits; never merges or pushes')
    s.add_argument('--fetch-into', metavar='REPO', help='fetch them into REPO as branch fae-self/<date>')
    s = sub.add_parser('acceptance', parents=[home, setup_flags], help='setup, start, submit a task file, wait and write evidence')
    s.add_argument('--task', required=True, help='JSON file: {"title", "prompt", "declared_files"}')
    s.add_argument('--wait', type=int, default=4 * 3600, help='seconds to wait for Done or Escalated (default %(default)s)')
    s.add_argument('--poll', type=float, default=15, help='seconds between polls (default %(default)s)')
    s.add_argument('--evidence', help='evidence JSON path (default HOME/.self-host/acceptance-<time>.json)')
    s.add_argument('--keep-running', action='store_true', help='leave the instance running afterwards')
    s.add_argument('--drain-wait', type=int, default=1200, help=argparse.SUPPRESS)
    return p


def main(argv):
    a = parser().parse_args(argv)
    try:
        result = {'setup': setup, 'start': start, 'submit': submit, 'status': status, 'stop': stop, 'land': land, 'acceptance': acceptance}[a.command](a)
    except SelfHostError as e:
        print('self-host: %s' % e, file=sys.stderr)
        return 1
    return result if isinstance(result, int) else 0


# ---------------------------------------------------------------------------
# Self-test: the pure parts, against synthetic documents shaped like the
# recorded CLI output of a scripted instance.

def self_test():
    import unittest

    claude = [{'provider': 'claude', 'model': 'claude-opus-5-5', 'effort': 'medium'},
              {'provider': 'claude', 'model': 'claude-sonnet-5-5', 'effort': 'medium'},
              {'provider': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}]
    go_test = ['go', 'test', '-json', '-count=1', '-a', '-x', './...']

    def workflow():
        return {'schema_version': 2, 'id': 'default', 'start': 'intake',
                'roles': {'coder': {'routes': json.loads(json.dumps(claude)), 'outputs': ['ImplementationReport']},
                          'oracle': {'routes': [{'provider': 'codex', 'model': 'gpt-6-astra', 'effort': 'low'}] + json.loads(json.dumps(claude))}},
                'stages': {'implement': {'primitive': 'AgentTurn', 'role': 'coder', 'command': ['not', 'a', 'host', 'command']},
                           'run-red': {'primitive': 'HostCommand', 'command': list(go_test)},
                           'qa': {'primitive': 'HostCommand', 'command': list(go_test)},
                           'format': {'primitive': 'HostCommand', 'command': ['gofmt', '-l', '.']}},
                'edges': [{'from': 'intake', 'to': 'implement'}], 'limits': {'attempts': 3}}

    sha = 'a' * 40

    def config(**overrides):
        args = dict(base=sha, bridge='/b/fae-claude-bridge', claude='/c/claude', claude_config_dir='/h/.claude',
                    provider_timeout=DEFAULT_PROVIDER_TIMEOUT, workflow=workflow(), host_ledger='/h/.fae/host-ledger')
        args.update(overrides)
        return live_config(**args)

    class ConfigTests(unittest.TestCase):
        def test_no_task_scope_pinned(self):
            c = config()
            self.assertEqual(c['files'], [])
            self.assertEqual(c['commit_message'], '')
            for key in ('test_file', 'test_id', 'expected_failure', 'tasks'):
                self.assertNotIn(key, c)

        def test_allowlist_is_exactly_the_host_command_vectors(self):
            self.assertEqual(config()['allowed_commands'], [list(go_test), ['gofmt', '-l', '.']])

        def test_environment_holds_only_claude_config_dir(self):
            self.assertEqual(config()['environment'], {'CLAUDE_CONFIG_DIR': '/h/.claude'})

        def test_command_and_pinned_values(self):
            c = config()
            self.assertEqual(c['command'], ['/b/fae-claude-bridge', '--claude', '/c/claude', '--config', '{config}', '--timeout', '840s'])
            self.assertEqual((c['base'], c['integration_ref'], c['adapter']), (sha, 'refs/heads/fae/integration', 'direct'))
            self.assertEqual((c['heavy_capacity'], c['timeout_seconds'], c['stall_seconds'], c['stall_safe_replay']), (2, 900, 300, True))
            self.assertEqual((c['nested_commands'], c['max_output_bytes']), ('account', 16777216))
            self.assertNotIn('herdr_binary', c)

        def test_herdr_selects_the_multiplexer(self):
            c = config(herdr='/h/bin/herdr-core')
            self.assertEqual((c['adapter'], c['herdr_binary']), ('multiplexer', '/h/bin/herdr-core'))

        def test_refusals(self):
            for bad in (dict(base='abc'), dict(bridge='bin/bridge'), dict(claude_config_dir='~/.claude'), dict(provider_timeout=0)):
                with self.assertRaises(SelfHostError):
                    config(**bad)

    class RouteTests(unittest.TestCase):
        def test_matching_first_route_keeps_the_provider_chain(self):
            narrowed = narrow_routes(workflow(), 'claude', 'claude-opus-5-5')
            self.assertEqual(narrowed['roles']['coder']['routes'], claude[:2])

        def test_other_first_route_becomes_the_requested_binding(self):
            narrowed = narrow_routes(workflow(), 'claude', 'claude-opus-5-5')
            self.assertEqual(narrowed['roles']['oracle']['routes'], [{'provider': 'claude', 'model': 'claude-opus-5-5', 'effort': 'medium'}])
            narrowed = narrow_routes(workflow(), 'claude', 'claude-sonnet-5-5')
            self.assertEqual(narrowed['roles']['coder']['routes'], [{'provider': 'claude', 'model': 'claude-sonnet-5-5', 'effort': 'medium'}])

        def test_nothing_else_changes(self):
            original = workflow()
            narrowed = narrow_routes(original, 'claude', 'claude-opus-5-5')
            self.assertEqual(original, workflow(), 'input modified')
            for doc in (original, narrowed):
                for role in doc['roles'].values():
                    del role['routes']
            self.assertEqual(narrowed, original)

    class HomeTests(unittest.TestCase):
        def test_bound(self):
            fits = '/h' + 'x' * (WORKTREES_MAX_BYTES - len('/h/self/worktrees'))
            self.assertEqual(len(check_home(fits)), WORKTREES_MAX_BYTES)
            with self.assertRaises(SelfHostError) as refused:
                check_home(fits + 'y')
            self.assertIn('108 bytes', str(refused.exception))
            self.assertIn('at most 25 bytes', str(refused.exception))

        def test_default_shape_fits_a_home_directory_of_14_bytes(self):
            self.assertEqual(len(check_home('/home/operator/fae-self/i')), 40)
            with self.assertRaises(SelfHostError):
                check_home('/home/operator1/fae-self/i')

        def test_non_canonical_refused(self):
            for bad in ('fae-self/i', '/a/../b', '/a/./b', '/a/b/'):
                with self.assertRaises(SelfHostError):
                    check_home(bad)

        def test_multibyte_paths_count_bytes(self):
            with self.assertRaises(SelfHostError):
                check_home('/h/' + '\u00e9' * 12)

    class SubmitTests(unittest.TestCase):
        def test_declared_files(self):
            self.assertEqual(clean_declared(['b_test.go', 'a.go', 'a.go']), ['a.go', 'b_test.go'])
            for bad in ('/abs.go', '../up.go', 'a//b.go', './a.go', '.git/config', 'a\\b.go'):
                with self.assertRaises(SelfHostError):
                    clean_declared([bad])
            self.assertEqual(declared_warnings(['a.go', 'a_test.go']), [])
            self.assertTrue(declared_warnings(['a.go']))
            self.assertTrue(declared_warnings(['a_test.go', 'b_test.go']))
            self.assertTrue(declared_warnings([]))

        def test_deferred_refusal(self):
            self.assertTrue(deferred_refusal('host admission: HostPressure'))
            self.assertTrue(deferred_refusal('x: retry control after current run'))
            self.assertFalse(deferred_refusal('host admission: HostPressureX'))
            self.assertFalse(deferred_refusal('authorizing message is too old'))

        def test_drain_settled(self):
            self.assertTrue(drain_settled({'drained': True, 'draining': True, 'active': 0}))
            self.assertTrue(drain_settled({'drained': False, 'draining': True, 'active': 0}))
            self.assertFalse(drain_settled({'drained': False, 'draining': True, 'active': 1}))
            self.assertFalse(drain_settled({'drained': False, 'draining': False, 'active': 0}))

        def test_parse_time(self):
            self.assertEqual(seconds_between('2026-10-26T10:29:31.848430123Z', '2026-10-26T10:29:33Z'), 1.152)
            self.assertEqual(seconds_between('2026-10-26T10:29:31+00:00', '2026-10-26T11:29:31+01:00'), 0.0)
            with self.assertRaises(ValueError):
                parse_time('yesterday')

    t0 = '2026-10-26T10:00:00.000000001Z'

    def at(seconds):
        return (parse_time(t0) + datetime.timedelta(seconds=seconds)).strftime('%Y-%m-%dT%H:%M:%S.%fZ')

    task_id = 'task-1'
    disp = lambda n: '%s/wfhash/%d' % (task_id, n)
    sess = lambda n: disp(n) + '/session/0'

    def ev(seq, kind, n, stage, seconds):
        return {'sequence': seq, 'kind': kind, 'job_id': task_id, 'timestamp': at(seconds),
                'payload': {'attempt': 1, 'dispatch_id': disp(n), 'session': sess(n), 'stage': stage, 'role': '', 'binding': ''}}

    events = [{'sequence': 1, 'kind': 'task_admitted', 'job_id': task_id, 'payload': {}, 'timestamp': at(0)},
              ev(2, 'session-launched', 5, 'implement', 1), ev(3, 'session-reaped', 5, 'implement', 61),
              ev(4, 'session-launched', 6, 'run-red', 62), ev(5, 'session-reaped', 6, 'run-red', 74.5),
              ev(6, 'session-launched', 9, 'qa', 80), ev(7, 'session-reaped', 9, 'qa', 172),
              ev(8, 'session-launched', 10, 'format', 173),
              # A live daemon also records events whose payload is a bare value.
              {'sequence': 9, 'kind': 'drive-rearmed', 'job_id': task_id, 'payload': True, 'timestamp': at(174)}]
    records = [{'transitions': [{'from': 'Pending', 'to': 'intake', 'attempt': 1, 'note': 'admitted', 'created_at': at(0)}],
                'artifacts': [{'kind': 'ImplementationReport', 'stage': 'implement', 'dispatch_id': disp(5), 'role': 'coder', 'produced_at': at(61), 'provider': 'claude', 'model': 'claude-opus-5-5'},
                              {'kind': 'EngineRun', 'stage': 'run-red', 'dispatch_id': disp(6), 'role': '', 'produced_at': at(74),
                               'output': {'exit_code': 1, 'executed_count': 1, 'matched_count': 1, 'expected_failure': True}}],
                'next_cursor': 9},
               {'transitions': [{'from': 'merge', 'to': 'Done', 'attempt': 2, 'note': 'completed x', 'created_at': at(200)}],
                'artifacts': [{'kind': 'EngineRun', 'stage': 'qa', 'dispatch_id': disp(9), 'role': '', 'produced_at': at(172),
                               'output': {'exit_code': 0, 'executed_count': 2400}}]}]
    sessions = [{'launch': {'session_id': sess(5), 'dispatch_id': disp(5), 'task_id': task_id, 'role': 'coder',
                            'binding': {'provider': 'claude', 'model': 'claude-opus-5-5'}},
                 'events': [{'type': 'system', 'subtype': 'init', 'session_id': 'x'},
                            {'type': 'result', 'subtype': 'success', 'is_error': False, 'duration_ms': 59000, 'num_turns': 1, 'total_cost_usd': 0.5}]},
                {'launch': {'session_id': 'other/1/session/0', 'dispatch_id': 'other/1', 'task_id': task_id, 'role': 'spec', 'binding': {'provider': 'claude', 'model': 'm'}},
                 'events': [{'type': 'system', 'subtype': 'init'}]}]
    log_text = '%s\tAdd the watchdog NaN guard\n' % ('b' * 40)

    def evidence(**overrides):
        args = dict(task={'id': task_id, 'title': 'T', 'prompt': 'P', 'declared_files': ['a.go', 'a_test.go']}, outcome='Done',
                    show={'id': task_id, 'state': 'Done', 'stage': 'Done', 'parked': False, 'in_flight': False},
                    records=merge_pages(records, ('transitions', 'artifacts', 'decisions')), events=events,
                    snapshot={'payload': {'job': {'attempts': 2, 'test_runs': 5, 'failures': {'qa': 1}}}}, workflow=workflow(),
                    sessions=sessions, provider_log=[{'sequence': 1, 'session': sess(5), 'line': 'ok'}, {'sequence': 2, 'session': 'task-10/x', 'line': 'ok'}],
                    landed=landed_summary(sha, 'b' * 40, log_text, {'b' * 40: ' a.go | 2 +-\n'}, ' 2 files changed\n'),
                    status={'pid': 7, 'adapter': 'direct', 'uptime_seconds': 9, 'snapshot': {'counts': {'Done': 1}, 'drive': {'alive': True}}},
                    timing={'started_at': at(0), 'finished_at': at(201)})
        args.update(overrides)
        return assemble_evidence(**args)

    class EvidenceTests(unittest.TestCase):
        def test_engine_runs_with_durations(self):
            runs = evidence()['engine_runs']
            self.assertEqual([r['stage'] for r in runs], ['run-red', 'qa', 'format'])
            self.assertEqual([r['wall_seconds'] for r in runs], [12.5, 92.0, None])
            self.assertEqual(runs[0]['result']['matched_count'], 1)
            self.assertEqual(runs[1]['command'], go_test)
            self.assertEqual((runs[2]['artifact'], runs[2]['reaped_at']), (None, None))

        def test_provider_turns(self):
            e = evidence()
            first = e['provider_turns'][0]
            self.assertEqual((first['session_id'], first['stage'], first['role'], first['completed'], first['duration_ms']),
                             (sess(5), 'implement', 'coder', True, 59000))
            self.assertEqual(e['provider_turn_counts'], {'launched': 2, 'completed': 1})
            self.assertEqual([r['sequence'] for r in e['provider_log']], [1])

        def test_history_summary(self):
            h = evidence()['history']
            self.assertEqual((h['state'], h['attempts'], h['test_runs'], h['failures']), ('Done', 2, 5, {'qa': 1}))
            self.assertEqual(h['admission_to_done_seconds'], 200.0)
            self.assertEqual(h['artifact_counts'], {'ImplementationReport': 1, 'EngineRun': 2})
            self.assertNotIn('escalation', h)
            fallback = evidence(snapshot=None)['history']
            self.assertEqual((fallback['attempts'], fallback['test_runs']), (2, 2))

        def test_escalation(self):
            e = evidence(outcome='Escalated', show={'id': task_id, 'state': 'Escalated', 'stage': 'Escalated', 'escalation': 'qa-failed'},
                         snapshot={'payload': {'job': {'escalation_kind': 'AttemptCap', 'escalation_detail': 'qa'}}})
            self.assertEqual(e['history']['escalation'], {'kind': 'AttemptCap', 'detail': 'qa', 'reason': 'qa-failed'})

        def test_landed_and_final_status(self):
            e = evidence()
            self.assertEqual(e['landed']['landed_commit'], 'b' * 40)
            self.assertEqual(e['landed']['commits'], [{'sha': 'b' * 40, 'subject': 'Add the watchdog NaN guard', 'stat': 'a.go | 2 +-'}])
            self.assertEqual(e['landed']['diff_stat'], '2 files changed')
            self.assertEqual(e['final_status']['instance']['counts'], {'Done': 1})
            self.assertEqual(e['final_status']['task']['state'], 'Done')
            unmoved = landed_summary(sha, sha, '', {}, '')
            self.assertEqual((unmoved['moved'], unmoved['landed_commit'], unmoved['commits']), (False, None, []))

        def test_status_rows(self):
            row = task_row({'id': 't', 'state': 'Escalated', 'stage': 'Escalated', 'escalation': 'why'},
                           {'payload': {'admission': {'title': 'Fix'}, 'job': {'attempts': 3, 'test_runs': 4, 'escalation_kind': 'K', 'escalation_detail': 'D'}}})
            self.assertEqual((row['attempts'], row['test_runs'], row['title'], row['escalation']), (3, 4, 'Fix', {'kind': 'K', 'detail': 'D', 'reason': 'why'}))
            self.assertIn('attempts=?', format_task_row(task_row({'id': 't', 'state': 'Done', 'stage': 'Done'})))

    suite = unittest.TestSuite()
    for case in (ConfigTests, RouteTests, HomeTests, SubmitTests, EvidenceTests):
        suite.addTests(unittest.defaultTestLoader.loadTestsFromTestCase(case))
    result = unittest.TextTestRunner(stream=sys.stderr, verbosity=1).run(suite)
    if not result.wasSuccessful():
        return 1
    print('self-host.py self-test: %d tests passed' % result.testsRun)
    return 0


if __name__ == '__main__':
    if sys.argv[1:] in (['--self-test'], ['--unittest']):
        sys.exit(self_test())
    sys.exit(main(sys.argv[1:]))
