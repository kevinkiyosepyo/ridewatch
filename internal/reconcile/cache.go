package reconcile

import (
	"sync"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// tripCacheMax bounds the engine's TripSchedule cache. MBTA publishes ~70k
// trips per schedule; at a few hundred bytes each the cap keeps the cache to a
// few MB while comfortably holding every trip active on a given day.
const tripCacheMax = 20000

// tripCache holds TripSchedule lookups for one schedule version at a time,
// including negative entries (nil) for trips the schedule does not know, so
// ADDED trips re-polled every cycle cost no repeat store round-trips. The
// whole cache is cleared when the version changes.
type tripCache struct {
	mu      sync.Mutex
	max     int
	version int64
	entries map[string]*domain.TripSchedule
}

func newTripCache(max int) *tripCache {
	return &tripCache{max: max, entries: make(map[string]*domain.TripSchedule)}
}

// setVersion pins the cache to a schedule version, dropping every entry when
// it differs from the current one.
func (c *tripCache) setVersion(v int64) {
	c.mu.Lock()
	if c.version != v {
		c.version = v
		c.entries = make(map[string]*domain.TripSchedule)
	}
	c.mu.Unlock()
}

// get returns the cached lookup for (version, trip). ok distinguishes a cached
// negative entry (nil, true) from a miss (nil, false).
func (c *tripCache) get(version int64, tripID string) (*domain.TripSchedule, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != version {
		return nil, false
	}
	ts, ok := c.entries[tripID]
	return ts, ok
}

// put stores one lookup result (nil = trip not in schedule), evicting an
// arbitrary entry when full.
func (c *tripCache) put(version int64, tripID string, ts *domain.TripSchedule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != version {
		return
	}
	if _, ok := c.entries[tripID]; !ok && len(c.entries) >= c.max {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[tripID] = ts
}
