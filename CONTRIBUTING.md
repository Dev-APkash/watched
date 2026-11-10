# Contributing

Thanks for your interest in Watched! Bug reports, fixes and small improvements
are welcome.

## Before you start

- **Bugs:** open an issue using the bug report form.
- **Features:** open an issue first to talk it over. Watched stays small on
  purpose (no accounts, no trackers, no third-party Go dependencies), so not
  every idea will fit.
- **Security problems:** see [SECURITY.md](SECURITY.md). Don't open a public issue.

## Development

You need [Go](https://go.dev/dl/) 1.25 or newer.

```sh
go test ./...
go build -o watched .
```

To run it locally, copy `.env.example` to `.env` and fill it in, then
`go run .` and open http://localhost:8080.

CI runs these on every pull request, so run them first:

```sh
gofmt -l .        # should print nothing
go vet ./...
go test -race ./...
```

## Pull requests

- Branch from `main` (or from `v1` for a fix to the lite edition).
- Keep each PR focused on one change, and add tests where it makes sense.
- Don't add third-party dependencies.
- Never commit a `.env` file, API keys or screenshots of your real library.
  Use the built-in sample shelf for screenshots.

By contributing, you agree that your work is released under the
[MIT License](LICENSE).
