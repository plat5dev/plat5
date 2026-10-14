package roles

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
	internalTokenHeader = "X-Plat5-Internal-Token"
	// pollTimeout is longer than the control plane's long-poll wait, so an idle
	// poll ends with 204, not a client timeout.
	pollTimeout   = 40 * time.Second
	maxRetryDelay = 10 * time.Second
)

// Follower keeps a Source current from the control plane:
// GET {CONTROL_PLANE_URL}/internal/roles?after={revision}.
type Follower struct {
	client   *http.Client
	url      string
	token    string
	src      *Source
	log      zerolog.Logger
	revision int64
}

func NewFollower(baseURL, internalToken string, src *Source, log zerolog.Logger) *Follower {
	return &Follower{
		client: &http.Client{Timeout: pollTimeout},
		url:    strings.TrimRight(baseURL, "/") + "/internal/roles",
		token:  internalToken,
		src:    src,
		log:    log,
	}
}

// Load waits until the first roles document is stored, retrying while the
// control plane is down or has none. With roles on, identity does not serve
// before this returns.
func (f *Follower) Load(ctx context.Context) error {
	delay := time.Second
	for {
		res, err := f.fetch(ctx, nil)
		switch {
		case err != nil:
			f.log.Warn().Err(err).Msg("waiting for the control plane's roles")
		case res.missing:
			f.log.Warn().Msg("waiting for a roles document: the control plane has none yet; apply roles.yml to it")
		case res.set != nil:
			f.src.Store(res.set)
			f.revision = res.revision
			f.log.Info().Int64("revision", res.revision).Int("roles", len(res.set.List())).Msg("roles loaded")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, maxRetryDelay)
	}
}

// Follow long-polls for changes until ctx ends. A document identity cannot
// use is skipped, and the current one stays.
func (f *Follower) Follow(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		after := f.revision
		res, err := f.fetch(ctx, &after)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.log.Warn().Err(err).Msg("roles poll failed; keeping current roles")
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, maxRetryDelay)
			continue
		}
		delay = time.Second
		if res.unchanged {
			continue
		}
		f.revision = res.revision
		switch {
		case res.missing:
			f.log.Error().Msg("the control plane has no roles document; keeping current roles")
		case res.set != nil:
			f.src.Store(res.set)
			f.log.Info().Int64("revision", res.revision).Int("roles", len(res.set.List())).Msg("roles reloaded")
		}
	}
}

type fetched struct {
	revision  int64
	set       *Set
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
	if f.token != "" {
		req.Header.Set(internalTokenHeader, f.token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fetched{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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

	var feed struct {
		Revision int64           `json:"revision"`
		Roles    json.RawMessage `json:"roles"`
	}
	if err := json.Unmarshal(body, &feed); err != nil {
		return fetched{}, fmt.Errorf("roles feed: %w", err)
	}
	set, err := ParseJSON(feed.Roles)
	if err != nil {
		// Move past it: the next change is still picked up.
		f.log.Error().Err(err).Int64("revision", feed.Revision).Msg("roles document refused; keeping current roles")
		return fetched{revision: feed.Revision}, nil
	}
	return fetched{revision: feed.Revision, set: set}, nil
}
