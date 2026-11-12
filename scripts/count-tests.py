#!/usr/bin/env python3
"""Reject empty and skipped-only Go test JSON streams, per package.

--forbid-skip PATTERN also rejects any test that skipped with output matching
PATTERN: CI installs Dolt and herdr-core, so a skip naming them is vacuous."""
import json
import re
import sys

def count(rows, forbid_skip=None):
    packages = {r['Package'] for r in rows if 'Package' in r}
    passed = [r for r in rows if r.get('Action') == 'pass' and r.get('Test')]
    covered = {r['Package'] for r in passed}
    if not passed or packages - covered:
        raise ValueError('vacuous test run: no passed tests in ' + ', '.join(sorted(packages - covered)))
    if any(r.get('Action') == 'fail' for r in rows):
        raise ValueError('test failure in stream')
    if forbid_skip:
        output = {}
        for r in rows:
            if r.get('Action') == 'output' and r.get('Test'):
                key = (r.get('Package'), r['Test'])
                output[key] = output.get(key, '') + r.get('Output', '')
        skipped = sorted(f"{r.get('Package')} {r['Test']}" for r in rows if r.get('Action') == 'skip' and r.get('Test') and re.search(forbid_skip, output.get((r.get('Package'), r['Test']), '')))
        if skipped:
            raise ValueError('tests skipped for a missing prerequisite: ' + ', '.join(skipped))
    return len(passed)

if sys.argv[1:] == ['--self-test']:
    good = {'Action': 'pass', 'Package': 'example', 'Test': 'TestGood'}
    assert count([good]) == 1
    for bad in ([], [{'Action':'skip', 'Package':'example', 'Test':'TestSkip'}],
                [good, {'Action':'pass', 'Package':'empty'}],
                [good, {'Action':'fail', 'Package':'example'}]):
        try:
            count(bad)
        except ValueError:
            continue
        raise AssertionError('firing control did not reject vacuous/failed stream')
    skip = [good, {'Action': 'output', 'Package': 'example', 'Test': 'TestDolt', 'Output': '    x_test.go:9: FAE_TEST_DOLT_BINARY required\n'},
            {'Action': 'skip', 'Package': 'example', 'Test': 'TestDolt'}]
    assert count(skip) == 1 and count(skip, 'herdr-core|herdr') == 1
    try:
        count(skip, 'FAE_TEST_DOLT_BINARY|herdr-core|herdr')
    except ValueError:
        pass
    else:
        raise AssertionError('firing control did not reject a forbidden skip')
    print('Test-count positive and refusal controls passed')
else:
    args, forbid = sys.argv[1:], None
    if args[:1] == ['--forbid-skip'] and len(args) == 3:
        forbid, args = args[1], args[2:]
    try:
        with open(args[0]) as stream:
            print(f'{count([json.loads(line) for line in stream], forbid)} test cases passed')
    except (ValueError, IndexError) as error:
        sys.exit(str(error))
