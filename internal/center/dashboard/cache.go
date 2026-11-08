package dashboard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jooservices/speedtest/internal/center/stats"
)

// Source builds a dashboard snapshot.
type Source interface {
	Snapshot(ctx context.Context, q stats.Query) (Snapshot, error)
	Series(ctx context.Context, q stats.Query, percent bool, maxPoints int) ([]stats.Series, error)
}

// Cached shares one snapshot between concurrent and repeated requests. The
// snapshot aggregates the whole fleet over a window, which is the most
// expensive read; owners polling several tabs should not multiply that cost.
// Requests inside one TTL step for the same window share one computation, so a
// snapshot is at most TTL old. Failed computations are never cached.
type Cached struct {
	Source Source
	TTL    time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	done    chan struct{}
	snap    Snapshot
	err     error
	expires time.Time
}

// Series is not cached; it passes straight through.
func (c *Cached) Series(ctx context.Context, q stats.Query, percent bool, maxPoints int) ([]stats.Series, error) {
	return c.Source.Series(ctx, q, percent, maxPoints)
}

// Snapshot returns a snapshot at most TTL old.
func (c *Cached) Snapshot(ctx context.Context, q stats.Query) (Snapshot, error) {
	if c.TTL <= 0 {
		return c.Source.Snapshot(ctx, q)
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	key := c.key(q)
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*entry{}
	}
	e := c.entries[key]
	if e != nil && !e.expires.IsZero() && now.After(e.expires) {
		e = nil
	}
	if e == nil {
		e = &entry{done: make(chan struct{})}
		c.entries[key] = e
		if len(c.entries) > 64 { // windows are few; drop stale ones rather than grow
			for k, v := range c.entries {
				if k != key && !v.expires.IsZero() && now.After(v.expires) {
					delete(c.entries, k)
				}
			}
		}
		c.mu.Unlock()
		// The computation is detached from the first caller so its cancellation does not fail the others.
		go func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			e.snap, e.err = c.Source.Snapshot(cctx, q)
			c.mu.Lock()
			if e.err != nil {
				delete(c.entries, key)
			} else {
				e.expires = now.Add(c.TTL)
			}
			c.mu.Unlock()
			close(e.done)
		}()
	} else {
		c.mu.Unlock()
	}
	select {
	case <-e.done:
		return e.snap, e.err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

func (c *Cached) key(q stats.Query) string {
	step := c.TTL
	r := func(t time.Time) int64 { return t.Truncate(step).UnixNano() }
	return fmt.Sprintf("%s|%v|%d|%d|%d|%d|%s|%s|%s", strings.Join(q.NodeIDs, ","), q.NodesSet,
		r(q.From), r(q.To), r(q.PreviousFrom), r(q.PreviousTo), q.Timezone, q.Bucket, q.Engine)
}
