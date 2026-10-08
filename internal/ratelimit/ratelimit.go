// Package ratelimit implementa um limitador "token bucket" por chave (ex.: IP).
package ratelimit

import (
	"math"
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter permite, por chave, `rate` requisições por segundo com rajadas de até `burst`.
type Limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
}

// New cria um Limiter. Se now for nil, usa time.Now.
func New(rate float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: float64(burst), now: now, buckets: make(map[string]*bucket)}
}

// Allow consome um token da chave. Se não houver, devolve quanto tempo falta para ter um.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, found := l.buckets[key]
	if !found {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	missing := (1 - b.tokens) / l.rate
	return false, time.Duration(missing * float64(time.Second))
}

// Sweep remove as chaves sem uso há mais que `idle` (evita crescimento infinito do mapa).
func (l *Limiter) Sweep(idle time.Duration) int {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for k, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, k)
			removed++
		}
	}
	return removed
}

// Len devolve quantas chaves estão sendo acompanhadas.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
