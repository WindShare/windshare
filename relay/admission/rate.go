package admission

import (
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"time"
)

const (
	maxTrackedRateKeys = 4096
	maxRateKeyBytes    = 128
	unknownSource      = "<unknown>"
)

func sourceKey(source string) string {
	if source == "" {
		return unknownSource
	}
	return boundedRateKey(source)
}

// boundedRateKey prevents an injected source policy from turning one admitted
// key into an attacker-sized map allocation. Short protocol keys remain human
// readable; unusually long identities retain collision-resistant isolation.
func boundedRateKey(key string) string {
	if len(key) <= maxRateKeyBytes {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "<sha256:" + base64.RawURLEncoding.EncodeToString(sum[:]) + ">"
}

type bucketClass struct {
	rate    Rate
	buckets map[string]*bucket
	recency *list.List
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	element    *list.Element
}

func newBucketClass(rate Rate) *bucketClass {
	return &bucketClass{rate: rate, buckets: make(map[string]*bucket), recency: list.New()}
}

func (c *bucketClass) bucket(key string, now time.Time) (*bucket, bool) {
	key = boundedRateKey(key)
	if b, ok := c.buckets[key]; ok {
		c.refill(b, now)
		c.recency.MoveToFront(b.element)
		return b, true
	}
	if len(c.buckets) >= maxTrackedRateKeys {
		if !c.evictReplenishedOldest(now) {
			return nil, false
		}
	}
	b := &bucket{tokens: float64(c.rate.Burst), lastRefill: now}
	b.element = c.recency.PushFront(key)
	c.buckets[key] = b
	return b, true
}

func (c *bucketClass) refill(b *bucket, now time.Time) {
	if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
		b.tokens = min(float64(c.rate.Burst), b.tokens+elapsed*c.rate.PerSecond)
		b.lastRefill = now
	}
}

// evictReplenishedOldest is O(1) and never resets a depleted limiter. If all
// tracked sources are still active, an untracked source is conservatively
// denied until the oldest bucket naturally replenishes.
func (c *bucketClass) evictReplenishedOldest(now time.Time) bool {
	element := c.recency.Back()
	if element == nil {
		return false
	}
	key := element.Value.(string)
	b := c.buckets[key]
	c.refill(b, now)
	if b.tokens < float64(c.rate.Burst) {
		return false
	}
	delete(c.buckets, key)
	c.recency.Remove(element)
	return true
}
