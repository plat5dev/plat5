package lifecycle

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// PollInterval is how often each identity process reads the log's head, so an
// event written through any replica reaches waiting readers of every replica
// within about that long.
const PollInterval = time.Second

// Head is the newest event id this process has seen, for waiting readers.
type Head struct {
	mu      sync.Mutex
	id      int64
	changed chan struct{} // closed when id moves forward
	stopped chan struct{} // closed when Follow returns
}

func NewHead() *Head {
	return &Head{changed: make(chan struct{}), stopped: make(chan struct{})}
}

// Current is the latest head this process has seen.
func (h *Head) Current() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}

// Advance moves the head forward and wakes waiting readers. Never backward.
func (h *Head) Advance(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if id > h.id {
		h.id = id
		close(h.changed)
		h.changed = make(chan struct{})
	}
}

// WaitPast waits until the head is past after, ctx ends, or Follow stops.
// True if the head is past after.
func (h *Head) WaitPast(ctx context.Context, after int64) bool {
	for {
		h.mu.Lock()
		id, changed := h.id, h.changed
		h.mu.Unlock()
		if id > after {
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		case <-h.stopped:
			return false
		}
	}
}

// Follow reads the head every interval until ctx ends. A failed read is
// logged and tried again at the next tick. When it returns, waiting readers
// stop waiting, so shutdown isn't held up by long-polls.
func (h *Head) Follow(ctx context.Context, read func(context.Context) (int64, error), interval time.Duration, log zerolog.Logger) {
	defer close(h.stopped)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if id, err := read(ctx); err == nil {
			h.Advance(id)
		} else if ctx.Err() == nil {
			log.Warn().Err(err).Msg("lifecycle log head poll failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
