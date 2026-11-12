#!/usr/bin/env python3
"""Test-environment hygiene for Fae's own suite (FAE-VER-006).

snapshot STATE     record the fixture roots that exist before a run
check STATE        fail on residue: anything left in $TMPDIR, a fixture root
                   created during the run that no live process uses, or a live
                   process still carrying this run's TMPDIR
hostile [--] CMD   run CMD with every Fae environment variable the sources
                   name planted with a wrong value (the allowlisted test
                   injections keep their deliberate values), in a fresh
                   on-disk TMPDIR, then apply the same residue census
--self-test        drive the census and the planting against synthetic inputs

Fixture roots are the directories tests create outside the checkout with
os.MkdirTemp under /var/tmp, /tmp or /private/tmp; their prefixes are
enumerated from the test sources, so a new fixture root is censused without
editing this file.
"""
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SKIP_DIRS = {'.git', '.cache', '.tools', 'bin', 'node_modules', 'verification'}
TEMP_ROOTS = ['/var/tmp', '/tmp', '/private/tmp']
# Explicit test injections scripts/test.sh keeps; never ambient configuration.
ALLOWED = {'FAE_TEST_DOLT_BINARY', 'FAE_TEST_HERDR_BINARY', 'FAE_TEST_QUIET_HOST_PRESSURE'}
# Fae variables planted even when no source names them yet.
BASELINE = {'FAE_HOME', 'FAE_INSTANCE', 'FAE_ROOT', 'FAE_SOCKET', 'FAE_WORKSPACE', 'FAE_HOST_LEDGER'}
NAME_RE = re.compile(r'"(FAE_[A-Z0-9_]*)')
TRUTHY_RE = re.compile(r'Getenv\("(FAE_[A-Z0-9_]+)"\)\s*[!=]=\s*"1"')
MKDIR_RE = re.compile(r'MkdirTemp\(\s*(?:"(?:/var/tmp|/tmp|/private/tmp)"|base)\s*,\s*"([^"]+)"\s*\)')
# Helpers that wrap MkdirTemp under a fixed temp root take the prefix as an argument.
ROOT_HELPER_RE = re.compile(r'\bplaneRoot\(\s*t\s*,\s*"([^"]+)"')


def go_sources(root, tests_only=False):
    for d, dirs, names in os.walk(root):
        dirs[:] = sorted(x for x in dirs if x not in SKIP_DIRS)
        for n in sorted(names):
            if n.endswith('_test.go' if tests_only else '.go'):
                path = os.path.join(d, n)
                with open(path, encoding='utf-8') as f:
                    yield path, f.read()


def fae_variables(sources):
    """Every FAE_* name (or FAE_*_ prefix) the sources mention, with a wrong value."""
    names, truthy = set(BASELINE), set()
    for _, text in sources:
        names.update(NAME_RE.findall(text))
        truthy.update(TRUTHY_RE.findall(text))
    planted = {}
    for n in sorted(names - ALLOWED):
        if n == 'FAE_':
            continue
        name = n + 'HOSTILE' if n.endswith('_') else n
        planted[name] = '1' if name in truthy else '/nonexistent/fae-hostile/' + name.lower()
    return planted


def fixture_prefixes(sources):
    prefixes = set()
    for _, text in sources:
        prefixes.update(MKDIR_RE.findall(text) + ROOT_HELPER_RE.findall(text))
    return sorted(prefixes)


def fixture_roots(prefixes, temp_roots=TEMP_ROOTS):
    found = {}
    for base in temp_roots:
        try:
            names = os.listdir(base)
        except OSError:
            continue
        for n in names:
            if any(n.startswith(p) for p in prefixes):
                path = os.path.join(base, n)
                try:
                    found[path] = os.lstat(path).st_ino
                except OSError:
                    pass
    return found


def boot_time():
    try:
        with open('/proc/stat') as f:
            for line in f:
                if line.startswith('btime '):
                    return int(line.split()[1])
    except OSError:
        pass
    return 0


def in_fixture_root(path, prefixes, temp_roots=TEMP_ROOTS):
    for base in temp_roots:
        if path.startswith(base + '/'):
            name = path[len(base) + 1:].split('/')[0]
            return any(name.startswith(p) for p in prefixes)
    return False


def processes():
    """pid -> (cwd, cmdline, environ entries, start epoch) for this user's live processes."""
    out, btime, tick = {}, boot_time(), os.sysconf('SC_CLK_TCK')
    for pid in os.listdir('/proc') if os.path.isdir('/proc') else []:
        if not pid.isdigit():
            continue
        p = '/proc/' + pid
        try:
            if os.stat(p).st_uid != os.getuid():
                continue
            cwd = os.readlink(p + '/cwd')
            with open(p + '/cmdline', 'rb') as f:
                cmd = f.read().replace(b'\0', b' ').decode(errors='replace')
            with open(p + '/environ', 'rb') as f:
                env = set(f.read().decode(errors='replace').split('\0'))
            with open(p + '/stat') as f:
                stat = f.read()
        except OSError:
            continue
        fields = stat[stat.rfind(')') + 2:].split()
        if fields[0] in ('Z', 'X'):
            continue
        out[int(pid)] = (cwd, cmd, env, btime + int(fields[19]) / tick)
    return out


def ancestors():
    pids, pid = {os.getpid()}, os.getpid()
    while pid > 1:
        try:
            with open(f'/proc/{pid}/stat') as f:
                stat = f.read()
        except OSError:
            break
        pid = int(stat[stat.rfind(')') + 2:].split()[1])
        pids.add(pid)
    return pids


def census(state, tmpdir, temp_roots=TEMP_ROOTS, procs=None, prefixes=None):
    """Return (residue lines, warning lines) for a run whose TMPDIR is tmpdir."""
    residue, warnings = [], []
    if tmpdir and os.path.isdir(tmpdir):
        left = [n for n in os.listdir(tmpdir) if n != 'xcrun_db']  # macOS compiler cache
        if left:
            residue.append(f'test scratch residue in {tmpdir}: {sorted(left)}')
    procs = processes() if procs is None else procs
    skip = ancestors()
    marker = 'TMPDIR=' + tmpdir if tmpdir else None
    prefixes = state['prefixes'] if prefixes is None else prefixes
    for pid, (cwd, cmd, env, start) in sorted(procs.items()):
        if pid in skip:
            continue
        if marker and marker in env:
            residue.append(f'process {pid} of this run still running: {cmd[:160]}')
            continue
        # A process started during the run that outlived its fixture root (for
        # example an orphaned private herdr-core server) is a leak even when its
        # environment no longer carries the run's TMPDIR.
        gone = cwd[:-len(' (deleted)')] if cwd.endswith(' (deleted)') else None
        hc_socket = next((e[len('HC_SOCKET='):] for e in env if e.startswith('HC_SOCKET=')), None)
        if gone and start >= state.get('time', float('inf')) and in_fixture_root(gone, prefixes, temp_roots):
            residue.append(f'process {pid} still running in a removed fixture root {gone}: {cmd[:160]}')
        elif hc_socket and start >= state.get('time', float('inf')) and in_fixture_root(hc_socket, prefixes, temp_roots) and not os.path.exists(os.path.dirname(hc_socket)):
            residue.append(f'process {pid} still running for removed fixture socket {hc_socket}: {cmd[:160]}')
    before = state['roots']
    for path, ino in sorted(fixture_roots(prefixes, temp_roots).items()):
        if before.get(path) == ino:
            continue
        users = [pid for pid, (cwd, cmd, _, _) in procs.items()
                 if pid not in skip and (cwd == path or cwd.startswith(path + '/') or path in cmd)]
        if users:
            warnings.append(f'fixture root {path} created during the run is in use by {sorted(users)[:5]} (not attributed to this run)')
        else:
            residue.append(f'fixture root left behind: {path}')
    return residue, warnings


def snapshot(path, temp_roots=TEMP_ROOTS):
    prefixes = fixture_prefixes(go_sources(ROOT, tests_only=True))
    state = {'prefixes': prefixes, 'roots': fixture_roots(prefixes, temp_roots), 'time': time.time()}
    with open(path, 'w') as f:
        json.dump(state, f)
    return state


def report(residue, warnings, label):
    for w in warnings:
        print('warning: ' + w)
    for r in residue:
        print('residue: ' + r, file=sys.stderr)
    if residue:
        print(f'{label} failed with {len(residue)} residue item(s)', file=sys.stderr)
        return 1
    print(f'{label} passed')
    return 0


def check(path):
    with open(path) as f:
        state = json.load(f)
    residue, warnings = census(state, os.environ.get('TMPDIR', ''))
    return report(residue, warnings, 'Temporary-directory, fixture-root and process census')


def hostile_env(base, planted):
    env = {k: v for k, v in base.items() if not k.startswith('FAE_') or k in ALLOWED}
    env.update(planted)
    return env


def hostile(argv):
    if argv[:1] == ['--']:
        argv = argv[1:]
    if not argv:
        print('hostile: a command is required', file=sys.stderr)
        return 2
    planted = fae_variables(go_sources(ROOT))
    parent = os.path.join(ROOT, '.cache')
    os.makedirs(parent, exist_ok=True)
    tmpdir = tempfile.mkdtemp(prefix='h.', dir=parent)
    state = {'prefixes': fixture_prefixes(go_sources(ROOT, tests_only=True)), 'time': time.time()}
    state['roots'] = fixture_roots(state['prefixes'])
    env = hostile_env(os.environ, planted)
    env['TMPDIR'] = tmpdir
    print(f'hostile environment: {len(planted)} Fae variables planted with wrong values; TMPDIR={tmpdir}')
    try:
        code = subprocess.call(argv, env=env, cwd=ROOT)
        # Give asynchronous children of the run (watchdogs, reaped helpers) a moment to exit.
        for _ in range(20):
            residue, warnings = census(state, tmpdir)
            if not any(r.startswith('process ') for r in residue):
                break
            time.sleep(0.5)
        status = report(residue, warnings, 'Hostile-environment residue census')
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)
    if code != 0:
        print(f'hostile-environment run failed with exit {code}', file=sys.stderr)
        return 1
    return status


def self_test():
    sources = [('a.go', 'x := os.Getenv("FAE_ALPHA")\nif os.Getenv("FAE_FLAG") == "1" {}\nconst P = "FAE_PREFIX_"\n'
                        'os.Getenv("FAE_TEST_DOLT_BINARY")\n'),
               ('a_test.go', 'os.MkdirTemp("/var/tmp", "fae-x-")\nos.MkdirTemp(base, "fy-")\nos.MkdirTemp(t.TempDir(), "no-")\nplaneRoot(t, "fz-", "i")\n')]
    planted = fae_variables(sources)
    assert planted['FAE_ALPHA'].startswith('/nonexistent/') and planted['FAE_FLAG'] == '1', planted
    assert 'FAE_PREFIX_HOSTILE' in planted and 'FAE_TEST_DOLT_BINARY' not in planted and 'FAE_HOME' in planted, planted
    env = hostile_env({'FAE_TEST_DOLT_BINARY': '/dolt', 'FAE_STALE': 'x', 'PATH': '/bin'}, planted)
    assert env['FAE_TEST_DOLT_BINARY'] == '/dolt' and 'FAE_STALE' not in env and env['PATH'] == '/bin', env
    assert fixture_prefixes(sources) == ['fae-x-', 'fy-', 'fz-'], fixture_prefixes(sources)
    with tempfile.TemporaryDirectory() as scratch:
        base, tmpdir = os.path.join(scratch, 'var'), os.path.join(scratch, 'run')
        os.mkdir(base)
        os.mkdir(tmpdir)
        os.mkdir(os.path.join(base, 'fae-x-old'))
        state = {'prefixes': ['fae-x-'], 'roots': fixture_roots(['fae-x-'], [base])}
        residue, _ = census(state, tmpdir, [base], {})
        assert residue == [], residue
        os.mkdir(os.path.join(base, 'fae-x-new'))
        os.mkdir(os.path.join(base, 'other-new'))
        open(os.path.join(tmpdir, 'leak'), 'w').close()
        residue, _ = census(state, tmpdir, [base], {})
        assert len(residue) == 2 and 'fae-x-new' in residue[1] and 'leak' in residue[0], residue
        busy = {99999999: (os.path.join(base, 'fae-x-new', 'ws'), 'go test', set(), 0)}
        residue, warnings = census(state, '', [base], busy)
        assert residue == [] and len(warnings) == 1, (residue, warnings)
        # A live process carrying this run's TMPDIR is residue; another run's is not.
        child = subprocess.Popen(['sleep', '30'], env={'PATH': os.environ.get('PATH', '/bin'), 'TMPDIR': tmpdir})
        try:
            mine = [r for r in census(state, tmpdir, [base])[0] if r.startswith('process ')]
            other = [r for r in census(state, tmpdir + '-other', [base])[0] if r.startswith('process ')]
            assert any(str(child.pid) in r for r in mine) and not other, (mine, other)
        finally:
            child.send_signal(signal.SIGKILL)
            child.wait()
        # A process started during the run whose fixture root was removed is residue.
        state['time'] = time.time() - 1
        orphan_root = os.path.join(base, 'fae-x-orphan')
        os.mkdir(orphan_root)
        orphan = subprocess.Popen(['sleep', '30'], cwd=orphan_root, env={'PATH': os.environ.get('PATH', '/bin')})
        try:
            time.sleep(0.2)
            os.rmdir(orphan_root)
            found = [r for r in census(state, tmpdir + '-other', [base])[0] if 'removed fixture root' in r]
            assert any(str(orphan.pid) in r for r in found), found
            state['time'] = time.time() + 60
            assert not [r for r in census(state, tmpdir + '-other', [base])[0] if 'removed fixture root' in r]
        finally:
            orphan.send_signal(signal.SIGKILL)
            orphan.wait()
    print('Test-hygiene census positive and refusal controls passed')
    return 0


def main(argv):
    if argv == ['--self-test']:
        return self_test()
    if len(argv) == 2 and argv[0] == 'snapshot':
        snapshot(argv[1])
        return 0
    if len(argv) == 2 and argv[0] == 'check':
        return check(argv[1])
    if argv[:1] == ['hostile']:
        return hostile(argv[1:])
    print(__doc__.strip().split('\n\n')[1], file=sys.stderr)
    return 2


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
