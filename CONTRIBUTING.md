# Contributing

- Open an issue before a large change. Small fixes can go straight to a pull request.
- Use `uv sync` to install the project and its development dependencies.
- Run the full check set before you push:
  `uv run ruff check . && uv run ruff format --check . && uv run pyright && uv run pytest -q`.
- Add a test for every behaviour change. The reconcile loop deletes cluster objects, so an untested branch is a risk.
- Keep the change small. Touch only what the change requires.
- Write commit messages in the imperative mood.
- Record user-visible changes in `CHANGELOG.md`.
- The project targets Python 3.12 and later.
- The project is licensed Apache-2.0. A contribution carries the same license.
