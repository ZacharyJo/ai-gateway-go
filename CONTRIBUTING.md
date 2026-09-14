# Contributing

Thanks for your interest in contributing to ai-gateway-go!

## Development

- Requires Go 1.26+
- `make build` builds the binary into `dist/`
- `make test` runs the test suite (`go test ./...`)

## Commit conventions

- Keep commit messages in **English**.
- Follow [Conventional Commits](https://www.conventionalcommits.org/) (e.g. `feat:`, `fix:`, `chore:`).
- Do not push directly to `main`; all changes land via pull requests.

## Pull request process

1. Create a feature branch off `main`.
2. Make your changes, with tests for new behavior.
3. Open a pull request against `main` with a clear description.
4. After review, the PR is squash-merged into `main`.

## Reporting issues

When filing an issue, include: the version/commit you're on, your OS, and a minimal reproducer (config + request/response) if possible.
