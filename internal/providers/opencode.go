package providers

import (
	"context"
	"errors"

	"ai-usage-tracker/internal/core"
)

// PushProvider is fed by a client that POSTs usage to /api/ingest instead of
// being polled. Used for tools that keep usage on the machine they run on.
type PushProvider interface {
	Provider
	// PushCommand is the client subcommand that feeds this provider, e.g. "opencode".
	PushCommand() string
}

// openCode receives usage from `ai-usage-tracker push opencode`, which reads
// OpenCode's local session database. It covers every provider OpenCode talks
// to (OpenCode Zen, OpenRouter, Anthropic, …), with models labelled provider/model.
type openCode struct{}

func init() { register(openCode{}) }

func (openCode) ID() string          { return "opencode" }
func (openCode) Label() string       { return "OpenCode (local sessions)" }
func (openCode) Kind() string        { return KindPush }
func (openCode) PushCommand() string { return "opencode" }

func (openCode) Fetch(context.Context, *core.Credentials) (*core.Result, error) {
	return nil, errors.New("push-only provider: run `ai-usage-tracker push opencode`")
}
