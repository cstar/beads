//go:build !cgo

package beads

import (
	"context"
	"fmt"
)

func openEmbeddedReadyReader(_ context.Context, _, _ string) (ReadyReader, error) {
	return nil, fmt.Errorf("embedded Dolt requires CGO; use server mode (bd init --server)")
}
