//go:build !unix

package codex

import (
	"context"
	"time"
)

// No cross-process locking here; the in-process mutex still applies. Running
// this next to a live codex on such a platform can race on token refresh.
func lockFile(context.Context, string, time.Duration) (func(), error) {
	return func() {}, nil
}
