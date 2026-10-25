package nodes

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// SweepInterval is how often the sweeper runs.
const SweepInterval = time.Minute

// Sweeper is the tree's background work: the purge of nodes past
// purge_after, then any work added with Also (invite expiry).
type Sweeper struct {
	store *Store
	log   zerolog.Logger
	also  []sweepTask
}

type sweepTask struct {
	name string
	run  func(context.Context) (int, error)
}

// Also adds work to each sweep, after the purge. run returns how many things
// it changed, for the log.
func (w *Sweeper) Also(name string, run func(context.Context) (int, error)) {
	w.also = append(w.also, sweepTask{name: name, run: run})
}

func NewSweeper(store *Store, log zerolog.Logger) *Sweeper {
	return &Sweeper{store: store, log: log}
}

// Run sweeps until ctx ends, starting with one sweep at once.
func (w *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		w.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep runs once. A failure is logged and retried on the next run.
func (w *Sweeper) Sweep(ctx context.Context) {
	n, err := w.store.PurgeDue(ctx)
	if err != nil && ctx.Err() == nil {
		w.log.Error().Err(err).Msg("purge sweep failed")
	}
	if n > 0 {
		w.log.Info().Int("nodes", n).Msg("purged nodes")
	}
	for _, t := range w.also {
		n, err := t.run(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Error().Err(err).Str("task", t.name).Msg("sweep failed")
		}
		if n > 0 {
			w.log.Info().Int("count", n).Str("task", t.name).Msg("swept")
		}
	}
}
