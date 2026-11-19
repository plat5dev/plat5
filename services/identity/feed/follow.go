package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	// pollTimeout is longer than the control plane's long-poll wait, so an idle
	// poll ends with 204, not a client timeout.
	pollTimeout   = 40 * time.Second
	maxRetryDelay = 10 * time.Second
	maxBody       = 8 << 20
)

// Follower keeps a Source current from the control plane:
// GET {CONTROL_PLANE_URL}/internal/identity?after={revision}.
type Follower struct {
	client     *http.Client
	url        string
	src        *Source
	log        zerolog.Logger
	revision   int64
	retryDelay time.Duration
}

func NewFollower(baseURL string, src *Source, log zerolog.Logger) *Follower {
	return &Follower{
		client:     &http.Client{Timeout: pollTimeout},
		url:        strings.TrimRight(baseURL, "/") + "/internal/identity",
		src:        src,
		log:        log,
		retryDelay: time.Second,
	}
}

func (f *Follower) baseDelay() time.Duration {
	if f.retryDelay > 0 {
		return f.retryDelay
	}
	return time.Second
}

func (f *Follower) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Load waits until the first snapshot is stored, retrying while the control
// plane is down or has no config. Identity doesn't listen before this returns.
func (f *Follower) Load(ctx context.Context) error {
	delay := f.baseDelay()
	for {
		res, err := f.fetch(ctx, nil)
		switch {
		case err != nil:
			f.log.Warn().Err(err).Msg("waiting for the control plane's config")
		case res.missing:
			f.log.Warn().Msg("waiting for a config: the control plane has none yet; apply plat5.yml to it")
		case res.snap != nil:
			f.src.Store(res.snap)
			f.revision = res.revision
			f.log.Info().Int64("revision", res.revision).Int("kinds", len(res.snap.Kinds)).Msg("config loaded")
			return nil
		}
		if !f.sleep(ctx, delay) {
			return ctx.Err()
		}
		delay = min(delay*2, maxRetryDelay)
	}
}

// Follow long-polls for changes until ctx ends. A snapshot identity can't use
// is skipped, and the current one stays.
func (f *Follower) Follow(ctx context.Context) {
	base := f.baseDelay()
	delay := base
	backoff := func() bool {
		if !f.sleep(ctx, delay) {
			return false
		}
		delay = min(delay*2, maxRetryDelay)
		return true
	}
	for ctx.Err() == nil {
		after := f.revision
		res, err := f.fetch(ctx, &after)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.log.Warn().Err(err).Msg("config poll failed; keeping current config")
			if !backoff() {
				return
			}
			continue
		}
		if res.unchanged {
			delay = base
			continue
		}
		switch {
		case res.missing:
			f.revision = 0
			f.log.Error().Msg("the control plane has no config; keeping current config")
			if !backoff() {
				return
			}
		case res.snap != nil:
			delay = base
			f.revision = res.revision
			f.src.Store(res.snap)
			f.log.Info().Int64("revision", res.revision).Int("kinds", len(res.snap.Kinds)).Msg("config reloaded")
		case res.revision == 0:
			if !backoff() {
				return
			}
		default:
			delay = base
			f.revision = res.revision
		}
	}
}

type fetched struct {
	revision  int64
	snap      *Snapshot
	missing   bool
	unchanged bool
}

func (f *Follower) fetch(ctx context.Context, after *int64) (fetched, error) {
	u := f.url
	if after != nil {
		u += "?" + url.Values{"after": {strconv.FormatInt(*after, 10)}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fetched{}, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fetched{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fetched{}, err
	}

	switch resp.StatusCode {
	case http.StatusNoContent:
		return fetched{unchanged: true}, nil
	case http.StatusNotFound:
		return fetched{missing: true}, nil
	case http.StatusOK:
	default:
		return fetched{}, fmt.Errorf("control plane answered %d", resp.StatusCode)
	}

	snap, err := Parse(body)
	if err != nil {
		// Move past it: the next change is still picked up.
		var head struct {
			Revision int64 `json:"revision"`
		}
		_ = json.Unmarshal(body, &head)
		f.log.Error().Err(err).Int64("revision", head.Revision).Msg("config refused; keeping current config")
		return fetched{revision: head.Revision}, nil
	}
	return fetched{revision: snap.Revision, snap: snap}, nil
}
