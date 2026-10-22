#!/usr/bin/env python3
"""Reject empty and skipped-only Go test JSON streams, per package."""
import json
import sys

def count(rows):
    packages = {r['Package'] for r in rows if 'Package' in r}
    passed = [r for r in rows if r.get('Action') == 'pass' and r.get('Test')]
    covered = {r['Package'] for r in passed}
    if not passed or packages - covered:
        raise ValueError('vacuous test run: no passed tests in ' + ', '.join(sorted(packages - covered)))
    if any(r.get('Action') == 'fail' for r in rows):
        raise ValueError('test failure in stream')
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
    print('Test-count positive and refusal controls passed')
else:
    try:
        with open(sys.argv[1]) as stream:
            print(f'{count([json.loads(line) for line in stream])} test cases passed')
    except (ValueError, IndexError) as error:
        sys.exit(str(error))
