package nodes

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/plat5dev/plat5/identity/feed"
)

// SweepInterval is how often the sweeper runs when nothing kicks it.
const SweepInterval = time.Minute

// Sweeper is the tree's background work: kind removals against the current
// config, then the purge of nodes past purge_after. It runs on a timer, and
// at once when kicked by a new config.
type Sweeper struct {
	store *Store
	src   *feed.Source
	log   zerolog.Logger
	kick  chan struct{}
}

func NewSweeper(store *Store, src *feed.Source, log zerolog.Logger) *Sweeper {
	return &Sweeper{store: store, src: src, log: log, kick: make(chan struct{}, 1)}
}

// Kick asks for a sweep now. It never blocks.
func (w *Sweeper) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
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
		case <-w.kick:
		}
	}
}

// Sweep runs once. A failure is logged and retried on the next run.
func (w *Sweeper) Sweep(ctx context.Context) {
	snap := w.src.Current()
	if snap == nil {
		return
	}
	if err := w.store.ReconcileKinds(ctx, snap); err != nil && ctx.Err() == nil {
		w.log.Error().Err(err).Int64("revision", snap.Revision).Msg("kind reconcile failed")
	}
	n, err := w.store.PurgeDue(ctx)
	if err != nil && ctx.Err() == nil {
		w.log.Error().Err(err).Msg("purge sweep failed")
	}
	if n > 0 {
		w.log.Info().Int("nodes", n).Msg("purged nodes")
	}
}
