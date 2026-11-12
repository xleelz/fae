# Python support

Python tools run through the same scoped HostCommand policy, heavy-operation
lease and process limits as the other build adapters. No dependency installer
runs automatically. The selected interpreter must already provide pytest;
Ruff is optional and must already be on PATH.

Supported baseline vectors:

```text
python3 -m compileall src
python3 -m py_compile src/module.py
ruff check src
ruff format --check src
ruff format src
pytest tests
python3 -m pytest tests
```

Every command needs explicit, literal worktree paths. Paths escaping through
`..`, absolute paths outside the worktree, symlinks and response files are
refused. Compilation forces a fresh serial compile (`-f -j 1`); Ruff uses its
isolated, uncached baseline and one Rayon worker. Inherited Python, pytest and
Ruff settings are cleared. Bytecode and tool caches live in broker-owned disk
scratch directories. Project pytest `addopts` cannot override the admitted
vector, plugin autoload is disabled, and caller-supplied worker, plugin, report,
configuration and pattern-selection flags are refused. Project test and
`conftest.py` code still execute as code; these adapters are command policy,
not an operating-system sandbox.

For an exact RED test, declare `test_shape: python-pytest` and a file-qualified
node ID such as `tests/test_math.py::TestMath::test_value[case-one]`. The adapter
replaces broad file/directory selectors with that single literal argument.
Parametrized tests must name the exact parameter case. A file, class, wildcard
or `-k` pattern does not identify one RED test.

The broker creates a private JUnit XML report, fixes the report root to the
command directory and selects xunit1 so file identity remains available.
Pytest's disk cache provider is disabled. Executed counts exclude skips,
expected failures and collection/setup/teardown errors. An expected RED
requires exactly one matching completed failure, pytest exit 1, no report
errors and the declared fragment in that test's failure text. Captured stdout,
other tests' failures, collection/usage errors, no-tests exit 5, output cutoff
and timeout cannot prove the expected failure. Whole-suite runs count completed
cases but cannot prove a pinned expected failure without a test ID.

Unsupported forms include arbitrary `python script.py`, `python -c`, pip/uv
installation, xdist/forked workers, alternate pytest module/entry-point aliases,
Ruff `--fix` and arbitrary Ruff configuration. Use explicitly supported vectors
rather than wrapping them in another interpreter or installer. Core role
wrappers cover pytest and Ruff; `python -m pytest` is a HostCommand form.

The flags and report conventions follow the official
[pytest output documentation](https://docs.pytest.org/en/stable/how-to/output.html),
[pytest JUnit implementation](https://docs.pytest.org/en/stable/_modules/_pytest/junitxml.html),
[Python compileall documentation](https://docs.python.org/3/library/compileall.html)
and [Ruff configuration documentation](https://docs.astral.sh/ruff/configuration/).
