package nodes

import (
	"bytes"
	"context"
	"testing"

	"github.com/rs/zerolog"
)

func TestSweeperReturnsEarlyOnCanceledContext(t *testing.T) {
	var logs bytes.Buffer
	w := NewSweeper(nil, zerolog.New(&logs))
	called := false
	w.Also("expire invites", func(context.Context) (int, error) {
		called = true
		return 0, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Sweep(ctx)
	if called {
		t.Fatal("Also task ran despite canceled context")
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected logs on canceled sweep: %s", logs.String())
	}
}
