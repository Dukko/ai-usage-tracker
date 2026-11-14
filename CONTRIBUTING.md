# Contributing

Bug reports, provider requests and pull requests are welcome.

## Development

You need Go (see the version in `go.mod`). There's no other tooling.

```sh
go run .                 # http://localhost:8080, setup code in the log
go test ./...
gofmt -l .               # should print nothing
```

Templates, CSS and fonts in `internal/web` are embedded in the binary, so restart `go run .` after editing them.

## Adding a provider

Each provider is one file in `internal/providers`. Implement `providers.Provider` plus whichever of
`APIKeyProvider`, `OAuthProvider`, `DeviceProvider` or `PushProvider` fits, and call `register` in `init()`.
Return usage as daily rows (`core.DailyUsage`, `core.DailyCost`) and limits as `core.Quota` with a window id
like `5h` or `7d` (that lets the dashboard forecast from a single reading). Add a test with a fake HTTP server,
like `openrouter_test.go`.

## Pull requests

- Keep the UI working without JavaScript; scripts only add conveniences.
- No new runtime dependencies without a good reason: the binary is meant to stay small and self-contained.
- Never log credentials, and never send them to the browser.
