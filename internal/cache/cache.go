// Package cache implements the exact-match response cache, backed by the
// SQLite store. Keys hash the canonical request (minus the stream flag) so
// streaming and non-streaming requests share entries. Caching is opt-in per
// request (X-Gateway-Cache: true) or per key (cache_default).
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Damianen/llm-gateway/internal/provider"
	"github.com/Damianen/llm-gateway/internal/store"
)

// Clock abstracts time for tests.
type Clock func() time.Time

// Cache reads and writes cached canonical responses.
type Cache struct {
	store  *store.Store
	ttl    time.Duration
	clock  Clock
	logger *slog.Logger
}

// New returns a Cache. A nil clock means time.Now.
func New(st *store.Store, ttl time.Duration, clock Clock, logger *slog.Logger) *Cache {
	if clock == nil {
		clock = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Cache{store: st, ttl: ttl, clock: clock, logger: logger}
}

// Key returns the cache key for a canonical request under the resolved
// gateway model name (so aliases share entries). The stream flag is cleared
// before hashing.
func Key(model string, req *provider.Request) string {
	clone := *req
	clone.Stream = false
	clone.Model = model
	raw, err := json.Marshal(&clone)
	if err != nil {
		// Canonical requests always marshal; be defensive anyway.
		raw = []byte(model)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Get returns the cached response for key, if present and fresh.
func (c *Cache) Get(ctx context.Context, key string) (*provider.Response, bool) {
	raw, ok, err := c.store.CacheGet(ctx, key, c.clock())
	if err != nil {
		c.logger.Error("cache get failed", "err", err)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	var resp provider.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		c.logger.Error("cache entry corrupt", "err", err)
		return nil, false
	}
	return &resp, true
}

// Set stores a response under key with the configured TTL.
func (c *Cache) Set(ctx context.Context, key, model string, resp *provider.Response) {
	raw, err := json.Marshal(resp)
	if err != nil {
		c.logger.Error("cache marshal failed", "err", err)
		return
	}
	if err := c.store.CacheSet(ctx, key, model, raw, c.clock(), c.ttl); err != nil {
		c.logger.Error("cache set failed", "err", err)
	}
}

// RunSweeper deletes expired entries every interval until ctx is done.
func (c *Cache) RunSweeper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := c.store.CachePurgeExpired(ctx, c.clock())
			if err != nil {
				if ctx.Err() == nil {
					c.logger.Error("cache sweep failed", "err", err)
				}
				continue
			}
			if n > 0 {
				c.logger.Debug("cache sweep", "purged", n)
			}
		}
	}
}
