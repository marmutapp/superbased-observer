# `pypi/` — PyPI distribution sources

This directory holds the source layout for the
[`superbased-observer`](https://pypi.org/project/superbased-observer/)
PyPI package. The release workflow
(`.github/workflows/npm-release.yml`) builds five platform-tagged
wheels here per `v*` tag push and uploads them to PyPI.

The actual package source lives in `pypi/observer/`. See
`pypi/observer/README.md` for the user-facing documentation
(rendered as the PyPI long description) and
`docs/plans/pypi-package-plan-2026-06-02.md` for the original design
plan.

Sibling: `npm/` (the same observer binary, distributed via npm).
