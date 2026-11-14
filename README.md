# AI Usage Tracker

A small self-hosted dashboard for keeping an eye on AI usage across accounts:

- **API accounts (admin key)**: daily token usage by model and daily cost, from the
  Anthropic Admin API and the OpenAI organization Usage/Costs APIs.
- **OpenRouter**: daily spend, per-model history (with a management key), key limits and credit balance.
- **Subscriptions (OAuth)**: rate-limit window usage (5-hour, weekly, …) for
  **Claude Pro/Max** and **ChatGPT Plus/Pro (Codex)**, connected with an OAuth login.
- **OpenCode Go / Console**: the Go plan's 5-hour, weekly and monthly limits, the plan name, and Console
  spend budgets (which cover Zen pay-as-you-go spend), via the OpenCode Console sign-in or an API key.
- **OpenCode (local sessions)**: usage from your local OpenCode sessions (every provider it talks to: OpenCode Zen/Go,
  OpenRouter, …), pushed to the tracker by a small client command.

One Go binary, SQLite storage, a server-rendered UI that works without JavaScript, and credentials encrypted at rest.

The UI has a sign-in page with admin and read-only viewer accounts, 19 editor themes (Dracula, Catppuccin, Gruvbox,
Nord, Tokyo Night, … in dark and light versions), spend forecasts ("balance lasts to ~Dec 21") and limit
forecasts ("hits 100% in about 50m, before it resets"), and a Settings page for alerts, polling, users and API tokens.

## Run

```sh
cp .env.example .env    # optional: set TRACKER_PASSWORD to create the admin up front
docker compose up -d --build
# → http://localhost:8080
```

Or without Docker: `go run .`

### First sign-in

- If `TRACKER_PASSWORD` is set and there are no users yet, an admin called `admin` (or `TRACKER_USERNAME`) is
  created with that password. Changing the variable later does nothing: change passwords in Settings.
- Otherwise the server prints a one-time **setup code** to its log (`docker logs ai-usage-tracker`). The first
  visit asks for that code before it lets anyone create the admin account, so a fresh install exposed to the
  internet can't be claimed by someone else.

Admins can add **viewer** users in Settings. Viewers see the dashboard and account pages and can change their own
theme and password, but can't add, refresh or delete accounts or change site settings.

### Exposing it publicly (Cloudflare Tunnel, reverse proxy)

- Stored API keys and OAuth tokens are never sent to the browser; pages only show usage numbers.
- Sign-in is rate limited per IP: after 5 wrong passwords that address waits 15 minutes. Behind a proxy every
  request seems to come from the proxy, so set `TRACKER_TRUST_PROXY=true` so the real client IP
  (`CF-Connecting-IP`, `X-Real-IP` or `X-Forwarded-For`) is used. Only set it when the port isn't reachable
  directly, since otherwise clients could fake those headers.
- Set `TRACKER_PUBLIC_URL=https://…` so session cookies are marked `Secure`.
- Pages send a strict Content-Security-Policy and can't be framed. Forms are protected against cross-site requests.

## Configuration

| Env var | Default | |
|---|---|---|
| `TRACKER_ADDR` | `:8080` | Listen address |
| `TRACKER_DATA_DIR` | `./data` | SQLite DB + generated encryption key |
| `TRACKER_PASSWORD` | *(none)* | Creates the first admin with this password if there are no users yet |
| `TRACKER_USERNAME` | `admin` | Username for that first admin |
| `TRACKER_POLL_INTERVAL` | `10m` | How often every account is polled (min `1m`). Once an admin saves an interval in Settings, that wins. |
| `TRACKER_PUBLIC_URL` | *(from request)* | Public base URL: used in push setup instructions, and `https://` marks cookies `Secure` |
| `TRACKER_TRUST_PROXY` | `false` | Trust `CF-Connecting-IP` / `X-Forwarded-*` from a reverse proxy (for rate limiting and HTTPS detection) |
| `TRACKER_SECRET_KEY` | *(none)* | Passphrase for encrypting credentials. If unset, a random key is written to `$TRACKER_DATA_DIR/secret.key`. Back that file up with the DB. |

## Adding accounts

### API usage (admin key)

Regular API keys cannot read usage, so you need an **admin key**:

- **Anthropic**: Console → Settings → Admin keys (`sk-ant-admin01-…`). Requires an organization.
- **OpenAI**: Platform → Organization → Admin keys (`sk-admin-…`).

The tracker pulls the last 30 days on every poll and upserts them, so late-arriving data corrects itself.

### OpenRouter

Any OpenRouter key works:

- **Management key** (Settings → Management keys): 30 days of per-model tokens, requests and cost from `/activity`.
- **Regular API key**: `/activity` isn't available to it, so the tracker records today's spend (`usage_daily`)
  on every poll and builds history from then on. If the key has a credit limit, it shows as a quota bar.

The remaining credit balance is shown when OpenRouter reports it.

### OpenCode Go / Console

**Connect** → *OpenCode Go / Console* starts the same device sign-in the OpenCode CLI uses: open the link,
check the code, approve, and pick a workspace. The page continues on its own. Each workspace is its own account.

You get:

- **Go limits**: 5-hour rolling, weekly and monthly usage, plus the plan (Go, Go Plus, Go Teams).
  An untouched 5-hour window shows no reset time, because OpenCode reports a placeholder there.
- **Console budgets**: each spend budget that applies to you, as `$spent of $limit` with its reset date.
  This is how Zen pay-as-you-go spend shows up. OpenCode has no API for the raw Zen credit balance, so to see
  per-model Zen spend, add the *OpenCode (local sessions)* push account below.

Alternatively, add an OpenCode API key under *API usage*. It reads the Go limits but not the plan name.
These are the endpoints OpenCode's own CLI reads (`packages/core/src/usage-limits.ts` in anomalyco/opencode),
and the same approach as [OpenUsage](https://github.com/robinebers/openusage). They are not a documented
public API. The Console URL can be overridden with `OPENCODE_CONSOLE_URL`.

### OpenCode local sessions (push)

OpenCode keeps usage in a local SQLite database (`~/.local/share/opencode/opencode.db`), so the machine
running OpenCode sends it to the tracker instead of being polled:

1. In the UI, go to **Add account → Local tool → OpenCode** and copy the token (shown once).
2. On the OpenCode machine, run the same binary in push mode:

   ```sh
   ai-usage-tracker push opencode -url http://tracker:8080 -token <token>          # once
   TRACKER_URL=http://tracker:8080 TRACKER_PUSH_TOKEN=<token> \
     ai-usage-tracker push opencode -every 10m                                    # keep running
   ai-usage-tracker push opencode -dry-run                                         # inspect payload
   ```

   Or use `go install` / `go build` on that machine. The database is opened read-only, and only daily totals
   per `provider/model` are sent (tokens, request count and cost). No prompts or session content leave the machine.

Push tokens are scoped to one account and can be reissued from the account page.
OpenCode reports cost per message as computed by OpenCode. Requests it routed through OpenRouter also appear
in an OpenRouter account, so don't add those two totals together.

### Subscriptions (OAuth)

Click **Connect with OAuth**, open the sign-in page, approve access, and paste back:

- **Claude**: the authentication code Claude shows you.
- **ChatGPT**: you'll land on a `localhost:1455/...` page that fails to load. That's expected:
  copy the whole URL from the address bar and paste it.

Tokens are refreshed automatically. You can also **import** the credentials file
from the vendor CLI (`~/.claude/.credentials.json` or `~/.codex/auth.json`), but the
tracker rotates the refresh token when it refreshes, which can sign that CLI out.
A separate OAuth login avoids that.

> **Caveat:** subscription tracking reuses the public OAuth clients and the internal usage endpoints that
> Claude Code and the Codex CLI use. These are **not documented public APIs**. They may change or break, and
> the provider's terms may not cover this use. Endpoint URLs can be overridden with env vars
> (`CLAUDE_OAUTH_TOKEN_URL`, `CLAUDE_USAGE_URL`, `CHATGPT_USAGE_URL`, … see `internal/providers/*_sub.go`).

## JSON API

`GET /api/accounts` returns every account with its latest quotas, or its cost and token totals.
It's handy for Home Assistant, a status bar, or other widgets. Create a read-only token in
Settings › API tokens and send it as `Authorization: Bearer <token>`; a signed-in browser session works too.
Tokens only open this endpoint, not the UI.

```json
[{"id":1,"name":"Max","provider":"claude-sub","plan":"max","last_poll":1763130000,
  "quotas":[{"window":"5h","used_percent":23,"resets_at":1763140000},{"window":"7d","used_percent":41,"resets_at":1763500000}]}]
```

`GET /healthz` is unauthenticated, for container health checks. `POST /api/ingest` is used by push clients
and authenticates with its own push token.

## Layout

```
main.go                     config + wiring
internal/providers/         one file per provider (anthropic_api, openai_api, openrouter, claude_sub, chatgpt_sub,
                            opencode_console, opencode)
internal/push/              push client: local data readers (OpenCode) + /api/ingest sender
internal/poller/            periodic polling, token-refresh persistence
internal/store/             SQLite schema + queries
internal/secrets/           AES-GCM credential encryption
internal/web/               handlers, sign-in and sessions, settings, themes, forecasts, templates and CSS
                            (embedded in the binary, including the Inter and Space Grotesk fonts, both SIL OFL)
```

Adding a provider means implementing `providers.Provider` (plus any of `OAuthProvider`, `DeviceProvider`,
`APIKeyProvider` or `PushProvider`)
and calling `register` in `init()`.
