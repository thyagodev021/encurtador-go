package ratelimit

import (
	"sync"
	"testing"
	"time"

	"github.com/thyagodev021/encurtador-go/internal/testclock"
)

func TestBurstThenDeny(t *testing.T) {
	clk := testclock.New()
	l := New(1, 3, clk.Now)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("requisição %d deveria passar (rajada)", i+1)
		}
	}
	ok, retry := l.Allow("a")
	if ok {
		t.Fatal("4ª requisição deveria ser negada")
	}
	if retry <= 0 || retry > time.Second {
		t.Fatalf("retryAfter = %v, esperado (0, 1s]", retry)
	}
}

func TestRefillOverTime(t *testing.T) {
	clk := testclock.New()
	l := New(2, 2, clk.Now) // 2 tokens/s
	l.Allow("a")
	l.Allow("a")
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("deveria estar vazio")
	}
	clk.Advance(500 * time.Millisecond) // +1 token
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("deveria ter reposto 1 token")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("só 1 token foi reposto")
	}
}

func TestRefillNeverExceedsBurst(t *testing.T) {
	clk := testclock.New()
	l := New(100, 2, clk.Now)
	l.Allow("a")
	clk.Advance(time.Hour)
	allowed := 0
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("a"); ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("passaram %d, esperado exatamente o burst (2)", allowed)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	clk := testclock.New()
	l := New(1, 1, clk.Now)
	l.Allow("a")
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("'a' deveria estar bloqueada")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("'b' não deveria ser afetada por 'a'")
	}
}

func TestSweepRemovesIdleKeys(t *testing.T) {
	clk := testclock.New()
	l := New(1, 1, clk.Now)
	l.Allow("old")
	clk.Advance(2 * time.Minute)
	l.Allow("new")
	if removed := l.Sweep(time.Minute); removed != 1 {
		t.Fatalf("removidas = %d, esperado 1", removed)
	}
	if l.Len() != 1 {
		t.Fatalf("Len = %d, esperado 1", l.Len())
	}
}

func TestConcurrentAllowNeverExceedsBurst(t *testing.T) {
	clk := testclock.New() // relógio parado: só o burst pode passar
	l := New(1, 50, clk.Now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("k"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("passaram %d, esperado exatamente 50", allowed)
	}
}
