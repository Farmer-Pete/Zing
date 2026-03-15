# Contributing to Zing

Contributions are welcome! Whether it's a bug fix, a new feature, or an improvement to existing functionality, we appreciate your help.

## Setup

After cloning the repo, install the pre-commit hooks:

```sh
uv sync --dev
uv run pre-commit install
```

This sets up both pre-commit (lint) and pre-push (tests) hooks automatically.

## Guidelines

- **Keep changes small and focused.** Each pull request should address a single concern. Avoid bundling unrelated changes together — it makes review easier and keeps the history clean.
- **Run Zing on your changes before submitting.** All contributions are expected to have been reviewed by Zing prior to opening a pull request. Address its feedback before requesting human review.

## Code of Conduct

Please read and follow our [Code of Conduct](CODE_OF_CONDUCT.md). We are committed to providing a welcoming and supportive environment for everyone.
