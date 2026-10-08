// Package testclock fornece um relógio manual para testes determinísticos.
package testclock

import (
	"sync"
	"time"
)

// Clock é um relógio que só avança quando pedimos.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// New cria um relógio começando em um instante fixo.
func New() *Clock {
	return &Clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

// Now devolve o instante atual do relógio.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance avança o relógio.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
