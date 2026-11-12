#!/usr/bin/env python3
"""Acceptance-ledger census for the formal checks shipped by scripts/verify.sh.

Every Gobra production guard and every Gomela/SPIN model that verify.sh runs must
have exactly one ledger entry (verification/ledger/*.json) recording the
requirements it supports, its production predicate, positive Go tests, its firing
control and its native daemon-path evidence (or the explicit string "unknown").
The census fails closed: an unrecognised check line, an unmapped check, a stale
entry, a missing file or function, an undefined test or an unknown requirement ID
is an error. Non-formal shipped checks (validator rules, shim refusals, hook
denials, landing and host admission gates, bridge refusal classes, the nested
git policy's refusals and the derived task scope's refusals) are census-checked
the same way: every rule enumerated from its source must be claimed by exactly
one native-check entry, and each claimed rule must appear verbatim in a cited
firing control, or, for a source with an in-test census (IN_TEST_CENSUS), a
cited firing control must read that source and enumerate its rules with each
of the same patterns (the test then fails unless every rule fires). Where a
pattern enumerates one call shape (CALL_TOKENS), every such call in its
source must have that shape. Generated corpora, refusal families and
redaction families are mapped by their tests (check.corpus) and are not
enumerated.
It records evidence; it does not certify any requirement.

Usage: check-ledger.py [--report] | --self-test
"""
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
VERIFY = 'scripts/verify.sh'
REQUIREMENTS = 'REQUIREMENTS.md'
LEDGER_DIR = 'verification/ledger'
SKIP_DIRS = {'.git', '.cache', '.tools', 'bin', 'node_modules'}
STATUS = {'native-evidenced', 'native-unknown'}
KEYS = {
    'gobra-guard': {'id', 'kind', 'check', 'requirements', 'unmapped_reason', 'production',
                    'positive', 'firing', 'native', 'status', 'limits'},
    'gomela-model': {'id', 'kind', 'check', 'requirements', 'unmapped_reason', 'production',
                     'positive', 'firing', 'native', 'status', 'limits'},
    'native-check': {'id', 'kind', 'category', 'check', 'requirements', 'unmapped_reason', 'production',
                     'positive', 'firing', 'native', 'status', 'limits'},
}
PREFIX = {'gobra-guard': 'gobra:', 'gomela-model': 'gomela:', 'native-check': 'check:'}
SUBKEYS = {
    'gobra-guard': {'check': {'source'}, 'production': {'path', 'function', 'call_sites'},
                    'positive': {'tests', 'note'}, 'firing': {'mutation', 'refusal_tests', 'note'}},
    'gomela-model': {'check': {'model', 'functions'}, 'production': {'path', 'function'},
                     'positive': {'tests', 'note'}, 'firing': {'negative_model', 'functions', 'note'}},
    'native-check': {'check': {'source', 'markers', 'corpus'}, 'production': {'path', 'function'},
                     'positive': {'tests', 'note'}, 'firing': {'refusal_tests', 'note'}},
}
# Non-formal check sources: category -> [(file, function or None, rule regex)].
# A rule's marker is its first capture group (else the whole match), i.e. the
# exact source text of the rule. Every marker must be claimed by one entry.
CENSUS = {
    'validator-rule': [('workflow/workflow.go', None, re.compile(r'(?m)^\s*bad\((.*)\)\s*$')),
                       ('workflow/red.go', None, re.compile(r'(?m)Issue\{(.*)\}\)\s*$')),
                       ('workflow/requests.go', 'validateRequests', re.compile(r'(?m)^\s*bad\((.*)\)\s*$'))],
    'shim-refusal': [('commandshim/shim.go', None, re.compile(r'(?m)refuse\((.*)\)\s*$'))],
    'host-guard-gate': [('hostguard/guard.go', 'admit', re.compile(r'"[A-Z][A-Za-z]+"')),
                        ('hostguard/hold.go', 'Record', re.compile(r'"[A-Z][A-Za-z]+"'))],
    # A landing gate's marker is its gate name literal (Gate: "x" or verdict("x", ...)).
    'landing-gate': [('controller/gates.go', None, re.compile(r'(?:\bGate: |\bverdict\()("[a-z-]+")'))],
    # A role shim refusal's marker is the argument text of a deny(refuse(...))
    # or deny(fmt.Errorf(...)) (exit 77), or the reason of a &LocalRefusal{...}
    # (exit 78, a Local task's push or remote change), in RunShim.
    'role-shim-refusal': [('commandshim/role_git.go', 'runRoleGit', re.compile(r'(?m)refuse\((.*)\)\s*$')),
                          ('commandshim/role_git.go', 'runRoleGit', re.compile(r'(?m)&LocalRefusal\{(.*)\}\s*$')),
                          ('commandshim/role.go', 'RunShim', re.compile(r'(?m)deny\(refuse\((.*)\)\)\s*$')),
                          ('commandshim/role.go', 'RunShim', re.compile(r'(?m)deny\(fmt\.Errorf\((.*)\)\)\s*$')),
                          ('commandshim/role.go', 'RunShim', re.compile(r'(?m)&LocalRefusal\{(.*)\}\s*$')),
                          ('commandshim/shim.go', 'InstallDenials', re.compile(r"printf '%s\\\\n' '([^']*)'")),
                          ('commandshim/local_denials.go', 'InstallLocalDenials', re.compile(r"printf '(fae: command refused in Local mode [^']*?)%s"))],
    # A pre-tool hook denial's marker is the reason expression BypassesShim returns.
    'hook-denial': [('commandshim/hook.go', 'BypassesShim', re.compile(r'(?m)^\s*return (.*), true\s*$'))],
    # A bridge refusal class's marker is its constant, as the runtime accepts it.
    'bridge-refusal-class': [('runtime/direct.go', 'bridgeRefusalClassKnown', re.compile(r'class == ([A-Za-z]+)'))],
    # Account mode's nested git policy: a refusal's marker is the argument
    # text of a refuse(...) (exit 77) or the reason of a &LocalRefusal{...}
    # (exit 78) in commandshim/accounted_git.go.
    'nested-git-refusal': [('commandshim/accounted_git.go', None, re.compile(r'(?m)refuse\((.*)\)\s*$')),
                           ('commandshim/accounted_git.go', None, re.compile(r'(?m)&LocalRefusal\{(.*)\}\s*$'))],
    # The derived task scope: a refusal's marker is the argument text of a
    # liveScopeRefusal(...) in daemon/live_scope.go or
    # daemon/live_red_subtests.go, or of an error liveRedTurnChange returns
    # (the files a test-first turn may change).
    'derived-scope-refusal': [('daemon/live_scope.go', None, re.compile(r'(?m)liveScopeRefusal\((.*)\)\s*$')),
                              ('daemon/live_red_subtests.go', None, re.compile(r'(?m)liveScopeRefusal\((.*)\)\s*$')),
                              ('daemon/live_scope.go', 'liveRedTurnChange',
                               re.compile(r'(?m)return (?:fmt\.Errorf|errors\.New)\((.*)\)\s*$'))],
}
# The call each of these census patterns enumerates. In a source (or
# function) a census pattern reads, every such call outside a comment and a
# func declaration must be one the pattern matches: a refusal written in
# another shape (split over lines, followed by a comment, two on a line)
# would otherwise escape the census, so the census refuses it.
CALL_TOKENS = {
    r'(?m)refuse\((.*)\)\s*$': r'\brefuse\(',
    r'(?m)&LocalRefusal\{(.*)\}\s*$': r'&LocalRefusal\{',
    r'(?m)liveScopeRefusal\((.*)\)\s*$': r'\bliveScopeRefusal\(',
    r'(?m)return (?:fmt\.Errorf|errors\.New)\((.*)\)\s*$': r'\b(?:fmt\.Errorf|errors\.New)\(',
}
# Census categories whose firing control is an in-test census rather than
# quoted markers: a cited refusal test reads the source file (by its base
# name, as a test in its package does) and enumerates it with each of the
# category's patterns for that file, failing unless every rule it finds
# fires.
IN_TEST_CENSUS = {'nested-git-refusal', 'derived-scope-refusal'}
# Categories mapped by their tests (check.corpus), not enumerated from a source:
# the generated corpora, and refusal and redaction families whose rules live in
# a source the census does not enumerate (so a rule added there goes
# unnoticed).
CORPUS_CATEGORIES = {'generated-corpus', 'refusal-family', 'redaction-family'}
REQ_RE = re.compile(r'^(?:>\s*)?\*\*(FAE-[A-Z]+-[0-9]+)\b', re.M)
TEST_RE = re.compile(r'^func (Test[A-Za-z0-9_]*)\(\s*\w+ \*testing\.T\s*\)', re.M)
JAVA_RE = re.compile(r'^(?:if )?java (?:-\S+ )*-jar "\$jar" -i (\S+) > (\S+) 2>&1')
SED_RE = re.compile(r"^sed '(s/.*/)' (\S+) > \.cache/([A-Za-z0-9_-]+)-negative\.go$")
GUARD_RE = re.compile(r"^verify_guard ([A-Za-z0-9_-]+) (\S+) '([^']*)' '([^']*)'$")
LOOP_RE = re.compile(r'^for model in ([A-Za-z0-9_ ]+); do$')
CP_RE = re.compile(r'^cp (verification/(gomela|negative)/[A-Za-z0-9_.]+) "\$run_dir/(source|negative)/[A-Za-z0-9_]+\.go"$')
LOOP_BODY = {'cp "verification/gomela/$model.go" "$run_dir/source/$model.go"',
             'cp "verification/negative/$model.go.txt" "$run_dir/negative/$model.go"'}


class Repo:
    """File access; the self-test substitutes an in-memory mapping."""

    def __init__(self, root=None, files=None):
        self.root, self.files = root, files

    def exists(self, path):
        if not isinstance(path, str) or not path or path.startswith('/') or '..' in path.split('/'):
            return False
        if self.files is not None:
            return path in self.files
        return os.path.isfile(os.path.join(self.root, path))

    def read(self, path):
        if self.files is not None:
            return self.files[path]
        with open(os.path.join(self.root, path), encoding='utf-8') as f:
            return f.read()

    def test_files(self):
        if self.files is not None:
            return sorted(p for p in self.files if p.endswith('_test.go'))
        found = []
        for d, dirs, names in os.walk(self.root):
            dirs[:] = sorted(x for x in dirs if x not in SKIP_DIRS)
            found += [os.path.relpath(os.path.join(d, n), self.root) for n in names if n.endswith('_test.go')]
        return sorted(found)


def unsed(expr):
    """Split a literal sed s/// expression into (pattern, replacement)."""
    parts, cur, i = [], '', 2
    while i < len(expr):
        c = expr[i]
        if c == '\\' and i + 1 < len(expr):
            cur += expr[i + 1]
            i += 2
            continue
        if c == '/':
            parts.append(cur)
            cur = ''
        else:
            cur += c
        i += 1
    if len(parts) != 2 or cur:
        raise ValueError('unsupported sed expression: ' + expr)
    return parts[0], parts[1]


def parse_verify(text):
    """Enumerate shipped checks. Unrecognised check-like lines are errors."""
    errors, gobra, mutations, positive, negative = [], {}, {}, {}, {}
    lines = [l.strip() for l in text.splitlines()]
    in_guard = in_loop = False
    for n, line in enumerate(lines, 1):
        if line.startswith('verify_guard() {'):
            in_guard = True
            continue
        if in_guard:
            in_guard = line != '}'
            continue
        if in_loop:
            if line == 'done':
                in_loop = False
            elif line not in LOOP_BODY:
                errors.append(f'{VERIFY}:{n}: unrecognised Gomela loop body: {line}')
            continue
        m = JAVA_RE.match(line)
        if m:
            source, log = m.groups()
            if source.startswith('.cache/'):
                continue
            lm = re.fullmatch(r'verification/logs/gobra-([A-Za-z0-9_-]+)\.log', log)
            if not lm:
                errors.append(f'{VERIFY}:{n}: unrecognised Gobra log name: {log}')
                continue
            gobra.setdefault(lm.group(1), source)
            continue
        m = SED_RE.match(line)
        if m:
            try:
                mutations[m.group(3)] = (m.group(2),) + unsed(m.group(1))
            except ValueError as error:
                errors.append(f'{VERIFY}:{n}: {error}')
            continue
        m = GUARD_RE.match(line)
        if m:
            name, source, original, weakened = m.groups()
            if name in gobra:
                errors.append(f'{VERIFY}:{n}: duplicate Gobra check {name}')
            gobra[name] = source
            mutations[name] = (source, original, weakened)
            continue
        m = LOOP_RE.match(line)
        if m:
            in_loop = True
            for stem in m.group(1).split():
                positive[f'verification/gomela/{stem}.go'] = n
                negative[f'verification/negative/{stem}.go.txt'] = n
            continue
        m = CP_RE.match(line)
        if m:
            (positive if m.group(2) == 'gomela' else negative)[m.group(1)] = n
            if (m.group(2) == 'gomela') != (m.group(3) == 'source'):
                errors.append(f'{VERIFY}:{n}: model copied to the wrong side: {line}')
            continue
        if not line.startswith('#') and ('"$jar"' in line or line.startswith(('verify_guard', 'for model')) or
                                         re.search(r'\$run_dir/(source|negative)/', line) or
                                         (line.startswith('sed ') and '-negative.go' in line)):
            errors.append(f'{VERIFY}:{n}: unrecognised check line: {line[:120]}')
    for name, source in gobra.items():
        if name not in mutations:
            errors.append(f'{VERIFY}: Gobra check {name} has no firing mutation')
        elif mutations[name][0] != source:
            errors.append(f'{VERIFY}: Gobra check {name} mutates {mutations[name][0]}, verifies {source}')
    for name in mutations:
        if name not in gobra:
            errors.append(f'{VERIFY}: mutation {name} has no verified Gobra check')
    return {'gobra': gobra, 'mutations': mutations, 'positive': positive, 'negative': negative}, errors


def func_body(text, name):
    m = re.search(r'^func (?:\([^)]*\) )?' + re.escape(name) + r'[\[(]', text, re.M)
    if not m:
        return None
    first = text[m.start():].split('\n', 1)[0]
    if first.count('{') and first.count('{') == first.count('}'):
        return first  # one-line function
    end = re.search(r'^}[ \t]*$', text[m.end():], re.M)  # gofmt closes a top-level body with a lone brace
    return text[m.start(): m.end() + (end.end() if end else len(text) - m.end())]


def is_str(v):
    return isinstance(v, str) and v.strip() != ''


def census_markers(repo, census):
    """Enumerate every non-formal rule marker per source file."""
    found, errors = {}, []
    for category, sources in census.items():
        for path, function, pattern in sources:
            if not repo.exists(path):
                errors.append(f'census: {category} source does not exist: {path}')
                continue
            text = repo.read(path)
            if function:
                text = func_body(text, function)
                if text is None:
                    errors.append(f'census: func {function} is not defined in {path}')
                    continue
            markers = found.setdefault(path, {})
            matches = list(pattern.finditer(text))
            for m in matches:
                markers[m.group(1) if m.groups() else m.group(0)] = category
            if not matches:
                errors.append(f'census: no {category} rules found in {path}' + (f' {function}' if function else ''))
            token = CALL_TOKENS.get(pattern.pattern)
            if token:
                calls = sum(len(re.findall(token, line)) for line in text.splitlines()
                            if not line.lstrip().startswith(('//', 'func ')))
                if calls != len(matches):
                    errors.append(f'census: {category} {path}' + (f' {function}' if function else '') +
                                  f': {calls} calls but {len(matches)} in the one shape the census enumerates '
                                  f'({pattern.pattern}): write each refusal as one call ending its line')
    return found, errors


def validate(repo, verify_text, requirement_text, entries, census=None, in_test=None):
    census = CENSUS if census is None else census
    in_test = IN_TEST_CENSUS if in_test is None else in_test
    categories = set(census) | CORPUS_CATEGORIES
    checks, errors = parse_verify(verify_text)
    req_ids = list(dict.fromkeys(REQ_RE.findall(requirement_text)))
    req_set = set(req_ids)
    tests, verification_only = {}, set()
    for path in repo.test_files():
        for name in TEST_RE.findall(repo.read(path)):
            tests.setdefault(name, []).append(path)
    for name, paths in tests.items():
        if all(p.startswith('verification/') for p in paths):
            verification_only.add(name)
    if not req_ids:
        errors.append(f'{REQUIREMENTS}: no requirement IDs found')
    markers, more = census_markers(repo, census)
    errors += more
    claimed = {}

    def need_file(eid, field, path):
        if not repo.exists(path):
            errors.append(f'{eid}: {field} file does not exist: {path!r}')
            return False
        return True

    def need_func(eid, field, path, name):
        if not is_str(name):
            errors.append(f'{eid}: {field}.function is empty')
            return None
        if not need_file(eid, field, path):
            return None
        body = func_body(repo.read(path), name)
        if body is None:
            errors.append(f'{eid}: {field}: func {name} is not defined in {path}')
        return body

    def need_tests(eid, field, names, allow_empty=False):
        if not isinstance(names, list) or (not names and not allow_empty):
            errors.append(f'{eid}: {field} must be a {"" if allow_empty else "non-empty "}list of Go test names')
            return
        for t in names:
            if not isinstance(t, str) or t not in tests:
                errors.append(f'{eid}: {field}: {t!r} is not defined as func {t}(t *testing.T) in any *_test.go')
            elif t in verification_only:
                errors.append(f'{eid}: {field}: {t} only exercises a verification model, not production code')

    seen, negatives_used = {}, {}
    for e in entries:
        if not isinstance(e, dict):
            errors.append(f'ledger: entry is not an object: {e!r:.80}')
            continue
        eid, kind = e.get('id'), e.get('kind')
        if not is_str(eid):
            errors.append(f'ledger: entry without id: {e!r:.80}')
            continue
        if eid in seen:
            errors.append(f'{eid}: duplicate ledger entry')
        seen[eid] = e
        if kind not in KEYS:
            errors.append(f'{eid}: unknown kind {kind!r}')
            continue
        if not eid.startswith(PREFIX[kind]):
            errors.append(f'{eid}: id must start with {PREFIX[kind]!r} for kind {kind}')
        for k in sorted(set(e) - KEYS[kind]):
            errors.append(f'{eid}: unknown field {k!r}')
        for k in sorted({'id', 'kind', 'check', 'requirements', 'production', 'positive', 'firing', 'native', 'status'} - set(e)):
            errors.append(f'{eid}: missing field {k!r}')
        for field, allowed in SUBKEYS[kind].items():
            if not isinstance(e.get(field, {}), dict):
                errors.append(f'{eid}: {field} must be an object')
                continue
            for k in sorted(set(e.get(field, {})) - allowed):
                errors.append(f'{eid}: unknown field {field}.{k}')
        name = eid[len(PREFIX[kind]):]
        check = e.get('check') if isinstance(e.get('check'), dict) else {}
        prod = e.get('production') if isinstance(e.get('production'), dict) else {}
        pos = e.get('positive') if isinstance(e.get('positive'), dict) else {}
        fire = e.get('firing') if isinstance(e.get('firing'), dict) else {}
        reqs = e.get('requirements')
        if not isinstance(reqs, list) or not all(isinstance(r, str) for r in reqs):
            errors.append(f'{eid}: requirements must be a list of IDs')
            reqs = []
        for r in reqs:
            if r not in req_set:
                errors.append(f'{eid}: {r!r} is not a requirement ID defined in {REQUIREMENTS}')
        if len(set(reqs)) != len(reqs):
            errors.append(f'{eid}: duplicate requirement IDs')
        if not reqs and not is_str(e.get('unmapped_reason')):
            errors.append(f'{eid}: no requirement IDs and no unmapped_reason')
        if reqs and 'unmapped_reason' in e:
            errors.append(f'{eid}: unmapped_reason given although requirements are mapped')
        need_tests(eid, 'positive.tests', pos.get('tests'))
        native = e.get('native')
        if native == 'unknown':
            want = 'native-unknown'
        elif isinstance(native, dict):
            want = 'native-evidenced'
            need_tests(eid, 'native.tests', native.get('tests'))
            if not is_str(native.get('path')):
                errors.append(f'{eid}: native.path must describe the daemon path')
            for k in sorted(set(native) - {'tests', 'path'}):
                errors.append(f'{eid}: unknown field native.{k}')
        else:
            want = None
            errors.append(f'{eid}: native must be "unknown" or {{"tests": [...], "path": "..."}}')
        if e.get('status') not in STATUS:
            errors.append(f'{eid}: status must be one of {sorted(STATUS)}')
        elif want and e.get('status') != want:
            errors.append(f'{eid}: status {e.get("status")!r} contradicts native evidence (expected {want!r})')
        if kind == 'native-check':
            category = e.get('category')
            if category not in categories:
                errors.append(f'{eid}: category must be one of {sorted(categories)}')
            need_func(eid, 'production', prod.get('path'), prod.get('function'))
            refusal = fire.get('refusal_tests')
            need_tests(eid, 'firing.refusal_tests', refusal)
            if category in CORPUS_CATEGORIES:
                need_tests(eid, 'check.corpus', check.get('corpus'))
                if 'source' in check or 'markers' in check:
                    errors.append(f'{eid}: a {category} entry names its tests in check.corpus, not source markers')
            elif category in census:
                source, rules = check.get('source'), check.get('markers')
                if source not in [p for p, _, _ in census[category]]:
                    errors.append(f'{eid}: check.source {source!r} is not a {category} census source')
                elif not isinstance(rules, list) or not rules or not all(is_str(r) for r in rules):
                    errors.append(f'{eid}: check.markers must list the rules it maps')
                else:
                    files = sorted({p for t in (refusal or []) if isinstance(t, str) for p in tests.get(t, [])})
                    bodies = ''.join(repo.read(p) for p in files)
                    if category in in_test:
                        # A cited test's body reads the source and applies each of its patterns.
                        base = '"' + source.rsplit('/', 1)[-1] + '"'
                        test_bodies = [func_body(repo.read(p), t) or '' for t in (refusal or []) if isinstance(t, str)
                                       for p in tests.get(t, [])]
                        for pattern in dict.fromkeys(c.pattern for path, _, c in census[category] if path == source):
                            if not any(base in b and pattern in b for b in test_bodies):
                                errors.append(f'{eid}: no firing control carries an in-test census of {source} '
                                              f'(reading {base} and enumerating it with {pattern})')
                    for r in rules:
                        if r not in markers.get(source, {}):
                            errors.append(f'{eid}: marker not found in {source}: {r}')
                        elif category not in in_test and r not in bodies:
                            errors.append(f'{eid}: no firing control names marker {r}')
                        claimed.setdefault((source, r), []).append(eid)
            continue
        if kind == 'gobra-guard':
            source = checks['gobra'].get(name)
            if source is None:
                errors.append(f'{eid}: no matching Gobra check in {VERIFY}')
            elif check.get('source') != source:
                errors.append(f'{eid}: check.source {check.get("source")!r} differs from {VERIFY} ({source})')
            if prod.get('path') != check.get('source'):
                errors.append(f'{eid}: production.path must be the Gobra-verified source')
            pred = prod.get('function')
            need_func(eid, 'production', prod.get('path'), pred)
            sites = prod.get('call_sites')
            if not isinstance(sites, list) or not sites:
                errors.append(f'{eid}: production.call_sites must list the production callers')
                sites = []
            for site in sites:
                site = site if isinstance(site, dict) else {}
                body = need_func(eid, 'production.call_sites', site.get('path'), site.get('function'))
                if body is not None and is_str(pred) and not re.search(r'(?<![A-Za-z0-9_])' + re.escape(pred) + r'\(', body):
                    errors.append(f'{eid}: call site {site.get("path")}:{site.get("function")} does not call {pred}')
            mut = fire.get('mutation') if isinstance(fire.get('mutation'), dict) else {}
            if not is_str(mut.get('original')) or 'weakened' not in mut:
                errors.append(f'{eid}: firing.mutation must record the verify.sh original/weakened text')
            elif name in checks['mutations']:
                _, original, weakened = checks['mutations'][name]
                if (mut.get('original'), mut.get('weakened')) != (original, weakened):
                    errors.append(f'{eid}: firing.mutation differs from {VERIFY}')
                elif repo.exists(check.get('source') or '') and repo.read(check['source']).count(original) != 1:
                    errors.append(f'{eid}: mutation original text does not occur exactly once in {check["source"]}')
            need_tests(eid, 'firing.refusal_tests', fire.get('refusal_tests'), allow_empty=True)
        else:
            model = check.get('model')
            if model != f'verification/gomela/{name}.go':
                errors.append(f'{eid}: check.model must be verification/gomela/{name}.go')
            if model not in checks['positive']:
                errors.append(f'{eid}: no matching Gomela positive model copied by {VERIFY}')
            for field, path, fns in (('check', model, check.get('functions')),
                                     ('firing', fire.get('negative_model'), fire.get('functions'))):
                if not isinstance(fns, list) or not fns:
                    errors.append(f'{eid}: {field}.functions must list the model functions')
                    continue
                for fn in fns:
                    need_func(eid, field, path, fn)
            neg = fire.get('negative_model')
            if neg not in checks['negative']:
                errors.append(f'{eid}: firing.negative_model {neg!r} is not a negative model copied by {VERIFY}')
            else:
                negatives_used.setdefault(neg, []).append(eid)
            need_func(eid, 'production', prod.get('path'), prod.get('function'))
    for source, rules in sorted(markers.items()):
        for r in sorted(rules):
            users = claimed.get((source, r), [])
            if len(users) != 1:
                errors.append(f'census: {rules[r]} {source}: {r} must be claimed by exactly one native-check entry, found {users}')
    for name, source in sorted(checks['gobra'].items()):
        if 'gobra:' + name not in seen:
            errors.append(f'{VERIFY}: Gobra check {name} ({source}) has no ledger entry gobra:{name}')
    for model in sorted(checks['positive']):
        stem = model.rsplit('/', 1)[-1][:-3]
        if 'gomela:' + stem not in seen:
            errors.append(f'{VERIFY}: Gomela model {model} has no ledger entry gomela:{stem}')
    for neg in sorted(checks['negative']):
        users = negatives_used.get(neg, [])
        if len(users) != 1:
            errors.append(f'{VERIFY}: negative model {neg} must be the firing control of exactly one entry, found {users}')
    return checks, req_ids, errors


def load_ledger(repo_root):
    entries, errors = [], []
    directory = os.path.join(repo_root, LEDGER_DIR)
    names = sorted(n for n in os.listdir(directory) if n.endswith('.json')) if os.path.isdir(directory) else []
    if not names:
        errors.append(f'{LEDGER_DIR}: no ledger files')
    for n in names:
        try:
            with open(os.path.join(directory, n), encoding='utf-8') as f:
                data = json.load(f)
        except (OSError, ValueError) as error:
            errors.append(f'{LEDGER_DIR}/{n}: {error}')
            continue
        if not isinstance(data, list):
            errors.append(f'{LEDGER_DIR}/{n}: top level must be a JSON array of entries')
            continue
        entries += data
    return entries, errors


def requirement_groups(text):
    """Separate accepted requirements from blockquoted proposals for reporting."""
    accepted = set(re.findall(r'^\*\*(FAE-[A-Z]+-[0-9]+)\b', text, re.M))
    proposed = set(re.findall(r'^>\s*\*\*(FAE-[A-Z]+-[0-9]+)\b', text, re.M)) - accepted
    return accepted, proposed


def summary(checks, req_ids, entries, requirement_text=""):
    mapped = sorted({r for e in entries for r in e.get('requirements', [])}, key=req_ids.index)
    unknown = [e['id'] for e in entries if e.get('native') == 'unknown']
    unmapped = [e['id'] for e in entries if not e.get('requirements')]
    native = [e for e in entries if e.get('kind') == 'native-check']
    by_category = {}
    for e in native:
        n = len(e.get('check', {}).get('markers') or e.get('check', {}).get('corpus') or [])
        by_category[e.get('category')] = by_category.get(e.get('category'), 0) + n
    accepted, proposed = requirement_groups(requirement_text)
    lines = [
        f'Ledger census: {len(checks["gobra"])} Gobra guards, {len(checks["positive"])} Gomela positive models, '
        f'{len(checks["negative"])} negative models; {len(entries)} entries',
        f'Non-formal checks: {len(native)} entries covering ' + ', '.join(f'{n} {c}' for c, n in sorted(by_category.items())),
        f'Accepted requirements with a supporting check (not acceptance): {len(set(mapped) & accepted)} of {len(accepted)}',
        f'Proposed requirements with a supporting check (not accepted): {len(set(mapped) & proposed)} of {len(proposed)}',
        f'Entries without a requirement (unmapped_reason given): {len(unmapped)}',
        f'Native daemon-path evidence unknown for {len(unknown)} entries: {", ".join(unknown) or "none"}',
    ]
    return lines


def report(req_ids, requirement_text, entries):
    titles = {}
    for m in re.finditer(r'^(?:>\s*)?\*\*(FAE-[A-Z]+-[0-9]+)\b([^\n]*)', requirement_text, re.M):
        titles.setdefault(m.group(1), m.group(2).replace('**', '').strip()[:60])
    by_req = {}
    for e in entries:
        for r in e.get('requirements', []):
            by_req.setdefault(r, []).append(e)
    accepted, proposed = requirement_groups(requirement_text)
    out = ['', 'Per-requirement evidence (support only; unmapped does not imply unimplemented):']
    for r in req_ids:
        es = by_req.get(r, [])
        if not es:
            out.append(f'{r}{" [PROPOSED]" if r in proposed else ""}  no mapped check; acceptance not assessed')
            continue
        native = sum(1 for e in es if e.get('native') != 'unknown')
        out.append(f'{r}{" [PROPOSED]" if r in proposed else ""}  {len(es)} check{"s" if len(es) != 1 else ""}, native evidence {native}/{len(es)}: '
                   + ', '.join(e['id'] + ('' if e.get('native') != 'unknown' else ' (native unknown)') for e in es))
    out.append('')
    out.append('Checks without a requirement mapping:')
    for e in entries:
        if not e.get('requirements'):
            out.append(f'{e["id"]}  {e.get("unmapped_reason", "")}')
    return out


def self_test():
    verify = '\n'.join([
        'java -Xss128m -jar "$jar" -i k/cap.go > verification/logs/gobra-cap.log 2>&1',
        "sed 's/return a \\&\\& b/return a/' k/cap.go > .cache/cap-negative.go",
        'if java -Xss128m -jar "$jar" -i .cache/cap-negative.go > verification/logs/gobra-negative.log 2>&1; then',
        'verify_guard() {',
        ' java -Xss128m -jar "$jar" -i "$source" > "verification/logs/gobra-$name.log" 2>&1',
        '}',
        "verify_guard pin k/pin.go 'return x' 'return true'",
        'for model in pin; do',
        ' cp "verification/gomela/$model.go" "$run_dir/source/$model.go"',
        ' cp "verification/negative/$model.go.txt" "$run_dir/negative/$model.go"',
        'done',
        'cp verification/gomela/cap.go "$run_dir/source/cap.go"',
        'cp verification/negative/cap_missing.go.txt "$run_dir/negative/cap.go"',
    ])
    reqs = '**FAE-X-001 (MUST)** one.\n**FAE-X-002 `cmd` (MUST)**\n> **FAE-X-003 (MUST)** quoted.\nmentions FAE-X-999 only in prose.\n'
    files = {
        'k/cap.go': 'package k\nfunc Cap(a, b bool) bool {\n\treturn a && b\n}\n',
        'k/pin.go': 'package k\nfunc Pin(x bool) bool {\n\treturn x\n}\n',
        'k/use.go': 'package k\nfunc (s *S) Use() bool {\n\treturn Cap(true, true) && Pin(true)\n}\n'
                    'func Wide(q interface {\n\tGet() bool\n}, ok bool) bool {\n\treturn Cap(q.Get(), ok)\n}\n'
                    'func One() bool { return true }\nfunc After() bool {\n\treturn Pin(false)\n}\n',
        'k/cap_test.go': 'package k\nfunc TestCap(t *testing.T) {}\nfunc TestNative(t *testing.T) {}\n',
        'verification/gomela/model_test.go': 'package m\nfunc TestModelOnly(t *testing.T) {}\n',
        'verification/gomela/cap.go': 'package m\nfunc CapModel() {\n}\n',
        'verification/gomela/pin.go': 'package m\nfunc PinModel() {\n}\n',
        'verification/negative/cap_missing.go.txt': 'package m\nfunc CapBroken() {\n}\n',
        'verification/negative/pin.go.txt': 'package m\nfunc PinBroken() {\n}\n',
        'k/rules.go': 'package k\nfunc Check(x int) {\n\tif x < 0 {\n\t\tbad("x", "negative")\n\t}\n\tbad("y", "other")\n}\n',
        'k/rules_test.go': 'package k\n// "x", "negative" and "y", "other"\nfunc TestRules(t *testing.T) {}\n',
        'k/refs.go': 'package k\nfunc Refs(x int) error {\n\tif x < 0 {\n\t\treturn refuse("below " + name(x))\n\t}\n\treturn refuse("odd")\n}\n',
        'k/refs_test.go': 'package k\nfunc TestRefsCensus(t *testing.T) {\n\traw, _ := os.ReadFile("refs.go")\n'
                          '\tsites := regexp.MustCompile(`(?m)refuse\\((.*)\\)\\s*$`).FindAll(raw, -1)\n}\n'
                          'func TestRefsElsewhere(t *testing.T) {\n\traw, _ := os.ReadFile("rules.go")\n'
                          '\tsites := regexp.MustCompile(`(?m)refuse\\((.*)\\)\\s*$`).FindAll(raw, -1)\n}\n',
    }
    census = {'validator-rule': [('k/rules.go', 'Check', re.compile(r'(?m)^\s*bad\((.*)\)\s*$'))],
              'counted-refusal': [('k/refs.go', None, re.compile(r'(?m)refuse\((.*)\)\s*$'))]}
    in_test = {'counted-refusal'}
    rule_markers = ['"x", "negative"', '"y", "other"']
    ref_markers = ['"below " + name(x)', '"odd"']

    def gobra(name, src, fn, orig, weak):
        return {'id': 'gobra:' + name, 'kind': 'gobra-guard', 'check': {'source': src}, 'requirements': ['FAE-X-001'],
                'production': {'path': src, 'function': fn, 'call_sites': [{'path': 'k/use.go', 'function': 'Use'}]
                               + ([{'path': 'k/use.go', 'function': 'Wide'}] if fn == 'Cap' else [])},
                'positive': {'tests': ['TestCap']}, 'firing': {'mutation': {'original': orig, 'weakened': weak}, 'refusal_tests': []},
                'native': {'tests': ['TestNative'], 'path': 'TestNative -> Use'}, 'status': 'native-evidenced'}

    def gomela(stem, neg, fn, nfn):
        return {'id': 'gomela:' + stem, 'kind': 'gomela-model',
                'check': {'model': f'verification/gomela/{stem}.go', 'functions': [fn]}, 'requirements': [],
                'unmapped_reason': 'extension', 'production': {'path': 'k/use.go', 'function': 'Use'},
                'positive': {'tests': ['TestCap']}, 'firing': {'negative_model': neg, 'functions': [nfn]},
                'native': 'unknown', 'status': 'native-unknown'}

    def native():
        return {'id': 'check:rules', 'kind': 'native-check', 'category': 'validator-rule',
                'check': {'source': 'k/rules.go', 'markers': list(rule_markers)}, 'requirements': ['FAE-X-001'],
                'production': {'path': 'k/rules.go', 'function': 'Check'}, 'positive': {'tests': ['TestRules']},
                'firing': {'refusal_tests': ['TestRules']}, 'native': 'unknown', 'status': 'native-unknown'}

    def counted():
        return {'id': 'check:refs', 'kind': 'native-check', 'category': 'counted-refusal',
                'check': {'source': 'k/refs.go', 'markers': list(ref_markers)}, 'requirements': ['FAE-X-001'],
                'production': {'path': 'k/refs.go', 'function': 'Refs'}, 'positive': {'tests': ['TestCap']},
                'firing': {'refusal_tests': ['TestRefsCensus']}, 'native': 'unknown', 'status': 'native-unknown'}

    def good():
        return [gobra('cap', 'k/cap.go', 'Cap', 'return a && b', 'return a'),
                gobra('pin', 'k/pin.go', 'Pin', 'return x', 'return true'),
                gomela('cap', 'verification/negative/cap_missing.go.txt', 'CapModel', 'CapBroken'),
                gomela('pin', 'verification/negative/pin.go.txt', 'PinModel', 'PinBroken'), native(), family(), counted()]

    def family():
        return {'id': 'check:family', 'kind': 'native-check', 'category': 'refusal-family',
                'check': {'corpus': ['TestRules']}, 'requirements': ['FAE-X-002'],
                'production': {'path': 'k/use.go', 'function': 'After'}, 'positive': {'tests': ['TestCap']},
                'firing': {'refusal_tests': ['TestRules']}, 'native': 'unknown', 'status': 'native-unknown'}

    repo = Repo(files=files)
    checks, ids, errors = validate(repo, verify, reqs, good(), census, in_test)
    assert not errors, errors
    assert ids == ['FAE-X-001', 'FAE-X-002', 'FAE-X-003'], ids
    assert sorted(checks['gobra']) == ['cap', 'pin'] and len(checks['positive']) == 2 == len(checks['negative']), checks

    def fires(label, mutate, verify_text=verify, expect=''):
        entries = good()
        mutate(entries)
        _, _, errs = validate(repo, verify_text, reqs, entries, census, in_test)
        if not errs or (expect and not any(expect in x for x in errs)):
            raise AssertionError(f'firing control did not reject: {label}: {errs}')

    def setter(i, path, value):
        def apply(entries):
            target = entries[i]
            for key in path[:-1]:
                target = target[key]
            target[path[-1]] = value
        return apply

    fires('shipped check missing from ledger', lambda es: es.pop(1), expect='has no ledger entry gobra:pin')
    fires('model missing from ledger', lambda es: es.pop(3), expect='has no ledger entry gomela:pin')
    fires('entry without matching check', lambda es: es.append(gobra('gone', 'k/cap.go', 'Cap', 'x', 'y')), expect='no matching Gobra check')
    fires('new guard added to verify.sh', lambda es: None, verify + "\nverify_guard pressure k/pin.go 'return x' 'return true'", 'gobra:pressure')
    fires('unrecognised check line', lambda es: None, verify + '\njava -jar "$jar" --input k/new.go', 'unrecognised')
    fires('hard-coded guard without ledger entry', lambda es: None,
          verify + '\njava -Xss256m -jar "$jar" -i k/pin.go > verification/logs/gobra-io.log 2>&1', 'gobra:io')
    fires('hard-coded guard without firing mutation', lambda es: es.append(gobra('io', 'k/pin.go', 'Pin', 'x', 'y')),
          verify + '\njava -Xss128m -jar "$jar" -i k/pin.go > verification/logs/gobra-io.log 2>&1', 'no firing mutation')
    fires('missing production file', setter(0, ['production', 'call_sites'], [{'path': 'k/nope.go', 'function': 'Use'}]), expect='does not exist')
    fires('undefined production function', setter(0, ['production', 'function'], 'Gone'), expect='not defined')
    fires('call site does not call guard', setter(1, ['production', 'call_sites'], [{'path': 'k/cap.go', 'function': 'Cap'}]), expect='does not call')
    fires('one-line body does not leak into next function', setter(1, ['production', 'call_sites'], [{'path': 'k/use.go', 'function': 'One'}]), expect='does not call')
    fires('undefined Go test', setter(0, ['positive', 'tests'], ['TestMissing']), expect='TestMissing')
    fires('model-only Go test', setter(0, ['native', 'tests'], ['TestModelOnly']), expect='verification model')
    fires('unknown requirement ID', setter(0, ['requirements'], ['FAE-X-999']), expect='FAE-X-999')
    fires('empty positive evidence', setter(0, ['positive', 'tests'], []), expect='positive.tests')
    fires('empty firing control', setter(0, ['firing'], {}), expect='firing.mutation')
    fires('firing mutation drift', setter(1, ['firing', 'mutation', 'weakened'], 'return false'), expect='differs')
    fires('empty negative model', setter(2, ['firing'], {}), expect='negative')
    fires('unmapped without reason', setter(3, ['unmapped_reason'], ''), expect='unmapped_reason')
    fires('status contradicts native', setter(0, ['status'], 'native-unknown'), expect='contradicts')
    fires('unknown native shape', setter(0, ['native'], 'probably'), expect='native must be')
    fires('negative reused', setter(3, ['firing', 'negative_model'], 'verification/negative/cap_missing.go.txt'), expect='exactly one')
    fires('unknown field', setter(0, ['coverage'], '100%'), expect='unknown field')
    fires('shipped model without entry', lambda es: None, verify + '\ncp verification/gomela/gone.go "$run_dir/source/gone.go"', 'gomela:gone')
    fires('unreferenced negative model', lambda es: None,
          verify + '\ncp verification/negative/extra.go.txt "$run_dir/negative/extra.go"', 'extra.go.txt')
    fires('undefined model function', setter(3, ['check', 'functions'], ['Absent']), expect='not defined')
    fires('missing negative model file', setter(2, ['firing', 'negative_model'], 'verification/negative/nope.go.txt'), expect='does not exist')
    fires('malformed call site', setter(0, ['production', 'call_sites'], ['k/use.go']), expect='call_sites')
    fires('mutation text absent from source', setter(1, ['firing', 'mutation', 'original'], 'return y'),
          verify.replace("'return x' 'return true'", "'return y' 'return true'"), 'exactly once')
    fires('unknown nested field', setter(0, ['firing', 'refusal_test'], []), expect='firing.refusal_test')
    fires('non-formal rule unclaimed', setter(4, ['check', 'markers'], rule_markers[:1]), expect='claimed by exactly one')
    fires('non-formal stale marker', setter(4, ['check', 'markers'], rule_markers + ['"z", "gone"']), expect='marker not found')
    fires('non-formal rule without firing control', setter(4, ['firing', 'refusal_tests'], ['TestCap']), expect='no firing control names')
    fires('non-formal empty firing control', setter(4, ['firing', 'refusal_tests'], []), expect='firing.refusal_tests')
    fires('non-formal unknown category', setter(4, ['category'], 'vibes'), expect='category must be')
    fires('non-formal rule claimed twice', lambda es: es.append(dict(native(), id='check:again')), expect='claimed by exactly one')
    fires('refusal family with source markers', setter(5, ['check'], {'corpus': ['TestRules'], 'markers': ['"x", "negative"']}), expect='names its tests in check.corpus')
    fires('refusal family without tests', setter(5, ['check'], {'corpus': []}), expect='check.corpus')
    fires('in-test census source without a census in its firing control', setter(6, ['firing', 'refusal_tests'], ['TestRules']),
          expect='in-test census')
    fires('in-test census of another source', setter(6, ['firing', 'refusal_tests'], ['TestRefsElsewhere']), expect='in-test census')
    fires('in-test census rule unclaimed', setter(6, ['check', 'markers'], ref_markers[:1]), expect='claimed by exactly one')
    fires('in-test census stale marker', setter(6, ['check', 'markers'], ref_markers + ['"gone"']), expect='marker not found')
    # A refusal written in a shape the census pattern does not enumerate.
    refs, refs_test = files['k/refs.go'], files['k/refs_test.go']
    for label, shape in (('a call followed by a comment', '\treturn refuse("odd") // note'),
                         ('a call split over lines', '\treturn refuse(\n\t\t"odd")')):
        files['k/refs.go'] = refs.replace('\treturn refuse("odd")', shape, 1)
        _, _, errs = validate(repo, verify, reqs, good(), census, in_test)
        if not any('in the one shape the census enumerates' in x for x in errs):
            raise AssertionError(f'firing control did not reject: {label}: {errs}')
    # A second pattern for one source: its rules are claimed, and the in-test
    # census must enumerate it too.
    files['k/refs.go'] = refs.replace('\treturn refuse("odd")', '\tif x == 2 {\n\t\treturn &LocalRefusal{"two"}\n\t}\n\treturn refuse("odd")', 1)
    two = dict(census, **{'counted-refusal': census['counted-refusal'] + [('k/refs.go', None, re.compile(r'(?m)&LocalRefusal\{(.*)\}\s*$'))]})
    _, _, errs = validate(repo, verify, reqs, good(), two, in_test)
    if not any('"two"' in x and 'claimed by exactly one' in x for x in errs):
        raise AssertionError(f'firing control did not reject: second-pattern rule without ledger entry: {errs}')
    claimed = good()
    claimed[6]['check']['markers'] = ref_markers + ['"two"']
    _, _, errs = validate(repo, verify, reqs, claimed, two, in_test)
    if not any('in-test census of k/refs.go' in x and 'LocalRefusal' in x for x in errs):
        raise AssertionError(f'firing control did not reject: in-test census missing a second pattern: {errs}')
    files['k/refs_test.go'] = refs_test.replace('.FindAll(raw, -1)\n}\nfunc TestRefsElsewhere',
                                                '.FindAll(raw, -1)\n\tlocal := regexp.MustCompile(`(?m)&LocalRefusal\\{(.*)\\}\\s*$`).FindAll(raw, -1)\n}\nfunc TestRefsElsewhere', 1)
    _, _, errs = validate(repo, verify, reqs, claimed, two, in_test)
    assert not errs, errs
    files['k/refs.go'], files['k/refs_test.go'] = refs, refs_test
    files['k/rules.go'] = files['k/rules.go'].replace('\n}\n', '\n\tbad("w", "new")\n}\n', 1)
    files['k/refs.go'] = files['k/refs.go'].replace('\treturn refuse("odd")', '\tif x == 1 {\n\t\treturn refuse("one")\n\t}\n\treturn refuse("odd")', 1)
    _, _, errs = validate(repo, verify, reqs, good(), census, in_test)
    if not any('"w", "new"' in x and 'claimed by exactly one' in x for x in errs):
        raise AssertionError(f'firing control did not reject: new rule without ledger entry: {errs}')
    if not any('"one"' in x and 'claimed by exactly one' in x for x in errs):
        raise AssertionError(f'firing control did not reject: new in-test census rule without ledger entry: {errs}')
    accepted, proposed = requirement_groups("**FAE-X-001 (MUST)** x\n> **FAE-X-002 (MUST)** y\n")
    assert accepted == {"FAE-X-001"} and proposed == {"FAE-X-002"}
    grouping = summary({"gobra": [], "positive": [], "negative": []},
                       ["FAE-X-001", "FAE-X-002"],
                       [{"id": "check:x", "requirements": ["FAE-X-002"], "native": "unknown"}],
                       "**FAE-X-001 (MUST)** x\n> **FAE-X-002 (MUST)** y\n")
    assert "0 of 1" in grouping[2] and "1 of 1" in grouping[3], grouping
    print('Ledger census positive and refusal controls passed')


def main(argv):
    if argv == ['--self-test']:
        self_test()
        return 0
    if argv not in ([], ['--report']):
        print(__doc__.strip().splitlines()[-1], file=sys.stderr)
        return 2
    repo = Repo(root=ROOT)
    entries, errors = load_ledger(ROOT)
    try:
        verify_text, requirement_text = repo.read(VERIFY), repo.read(REQUIREMENTS)
    except OSError as error:
        print(f'ledger census failed: {error}', file=sys.stderr)
        return 1
    checks, req_ids, more = validate(repo, verify_text, requirement_text, entries)
    errors += more
    if errors:
        for e in errors:
            print('ledger: ' + e, file=sys.stderr)
        print(f'ledger census failed with {len(errors)} errors', file=sys.stderr)
        return 1
    for line in summary(checks, req_ids, entries, requirement_text):
        print(line)
    if argv == ['--report']:
        for line in report(req_ids, requirement_text, entries):
            print(line)
    print('Ledger census passed (evidence census only; not requirement acceptance)')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
