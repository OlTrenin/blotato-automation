package httpapi

import (
	"sync"
	"time"
)

// Buckets exist only for configured tenants, never client-supplied identities.
// Quotas in PostgreSQL remain shared across replicas; this burst limit protects
// each process before it acquires a database connection.
type requestBucket struct {
	mu                     sync.Mutex
	rate, capacity, tokens float64
	last                   time.Time
}

func newRequestBucket(rate, burst int) *requestBucket {
	return &requestBucket{rate: float64(rate), capacity: float64(burst), tokens: float64(burst), last: time.Now()}
}
func (b *requestBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.capacity, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
