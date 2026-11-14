# Changelog

## [0.1.0] - 2026-11-14

First public release.

- Tracks subscription limits for Claude Pro/Max, ChatGPT Plus/Pro (Codex) and OpenCode Go, and daily spend and
  tokens for the Anthropic and OpenAI APIs, OpenRouter, OpenCode Console budgets and local OpenCode sessions.
- Sign-in with admin and read-only viewer users, a one-time setup code for new installs, and per-IP lockout.
- 19 editor themes in dark and light versions, follow-system mode and a compact layout.
- Spend forecasts (burn rate, month projection, when a balance or key limit runs out) and limit forecasts
  ("hits 100% before it resets") that work from the first check.
- Settings for check interval, history retention, warning levels, users, sessions and read-only API tokens.
- Multi-arch Docker image (amd64, arm64) and binaries for Linux, macOS and Windows.
