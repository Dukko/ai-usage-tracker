package providers

import (
	"context"
	"errors"

	"github.com/Dukko/ai-usage-tracker/internal/core"
)

// muse receives rate-limit snapshots from Muse, which prints them as text on
// its own machine and POSTs them to /api/usage/muse. There is no API to poll.
// The first successful push creates the account.
type muse struct{}

func init() { register(muse{}) }

func (muse) ID() string    { return "muse" }
func (muse) Label() string { return "Muse" }
func (muse) Kind() string  { return KindSnapshot }

func (muse) Fetch(context.Context, *core.Credentials) (*core.Result, error) {
	return nil, errors.New("snapshot-only provider: Muse pushes to /api/usage/muse")
}
