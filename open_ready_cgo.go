//go:build cgo

package beads

import (
	"context"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
)

func openEmbeddedReadyReader(ctx context.Context, dir, database string) (ReadyReader, error) {
	store, err := embeddeddolt.OpenReadOnly(ctx, dir, database, "main")
	if err != nil {
		return nil, err
	}
	return &readyReader{store: store}, nil
}
