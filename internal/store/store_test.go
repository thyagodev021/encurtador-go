package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thyagodev021/encurtador-go/internal/testclock"
)

func memStore(t *testing.T) (*Store, *testclock.Clock) {
	t.Helper()
	clk := testclock.New()
	s, err := Open(Options{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return s, clk
}

func fileStore(t *testing.T, path string, clk *testclock.Clock) *Store {
	t.Helper()
	s, err := Open(Options{Path: path, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutVisitLookup(t *testing.T) {
	s, _ := memStore(t)
	info, err := s.Put("abc", "https://example.com", 0)
	if err != nil || info.ID != "abc" || info.ExpiresAt != nil {
		t.Fatalf("Put = %+v, %v", info, err)
	}
	for i := 0; i < 3; i++ {
		url, err := s.Visit("abc")
		if err != nil || url != "https://example.com" {
			t.Fatalf("Visit = %q, %v", url, err)
		}
	}
	got, err := s.Lookup("abc")
	if err != nil || got.Clicks != 3 || got.LastClickAt == nil {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	// Lookup não conta clique
	got, _ = s.Lookup("abc")
	if got.Clicks != 3 {
		t.Fatalf("Lookup alterou os cliques: %d", got.Clicks)
	}
}

func TestNotFound(t *testing.T) {
	s, _ := memStore(t)
	if _, err := s.Visit("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Visit err = %v", err)
	}
	if _, err := s.Lookup("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup err = %v", err)
	}
	if err := s.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete err = %v", err)
	}
}

func TestDuplicateAlias(t *testing.T) {
	s, _ := memStore(t)
	if _, err := s.Put("x", "https://a.com", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("x", "https://b.com", 0); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, esperado ErrExists", err)
	}
	if url, _ := s.Visit("x"); url != "https://a.com" {
		t.Fatalf("o link original foi sobrescrito: %q", url)
	}
}

func TestExpirationAndReuseOfExpiredAlias(t *testing.T) {
	s, clk := memStore(t)
	s.Put("tmp", "https://old.com", time.Minute)
	if _, err := s.Visit("tmp"); err != nil {
		t.Fatalf("antes de expirar: %v", err)
	}
	clk.Advance(61 * time.Second)
	if _, err := s.Visit("tmp"); !errors.Is(err, ErrExpired) {
		t.Fatalf("Visit após expirar: %v", err)
	}
	if _, err := s.Lookup("tmp"); !errors.Is(err, ErrExpired) {
		t.Fatalf("Lookup após expirar: %v", err)
	}
	// um alias expirado pode ser reutilizado
	if _, err := s.Put("tmp", "https://new.com", 0); err != nil {
		t.Fatalf("reutilizar alias expirado: %v", err)
	}
	if url, _ := s.Visit("tmp"); url != "https://new.com" {
		t.Fatalf("url = %q", url)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, esperado 1", s.Len())
	}
}

func TestDelete(t *testing.T) {
	s, _ := memStore(t)
	s.Put("a", "https://a.com", 0)
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Visit("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d", s.Len())
	}
}

func TestSweepExpired(t *testing.T) {
	s, clk := memStore(t)
	for i := 0; i < 10; i++ {
		s.Put(fmt.Sprintf("short%d", i), "https://x.com", time.Minute)
	}
	s.Put("forever", "https://x.com", 0)
	if n := s.SweepExpired(); n != 0 {
		t.Fatalf("nada expirou ainda, mas removeu %d", n)
	}
	clk.Advance(2 * time.Minute)
	if n := s.SweepExpired(); n != 10 {
		t.Fatalf("removidos = %d, esperado 10", n)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, esperado 1", s.Len())
	}
}

func TestTopOrdersByClicks(t *testing.T) {
	s, clk := memStore(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		s.Put(id, "https://x.com/"+id, 0)
		clk.Advance(time.Second)
	}
	for i := 0; i < 5; i++ {
		s.Visit("c")
	}
	for i := 0; i < 2; i++ {
		s.Visit("a")
	}
	top := s.Top(3)
	var got []string
	for _, in := range top {
		got = append(got, in.ID)
	}
	// c (5), a (2), depois o mais novo entre os empatados com 0 cliques: d
	if strings.Join(got, ",") != "c,a,d" {
		t.Fatalf("Top = %v", got)
	}
	if len(s.Top(100)) != 4 {
		t.Fatal("Top(100) deveria devolver os 4")
	}
}

// ------------------------------------------------------------ concorrência (rode com -race)
func TestConcurrentVisitsCountExactly(t *testing.T) {
	s, _ := memStore(t)
	s.Put("hot", "https://x.com", 0)
	// Mais goroutines que núcleos aumenta a chance de expor incrementos não atômicos.
	// (Em máquinas de 1 núcleo esse tipo de erro quase nunca aparece; o CI tem vários.)
	prev := runtime.GOMAXPROCS(runtime.NumCPU() * 4)
	defer runtime.GOMAXPROCS(prev)
	const workers, each = 64, 2000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := s.Visit("hot"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	info, _ := s.Lookup("hot")
	if info.Clicks != workers*each {
		t.Fatalf("cliques = %d, esperado %d", info.Clicks, workers*each)
	}
}

func TestConcurrentPutSameIDExactlyOneWins(t *testing.T) {
	s, _ := memStore(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Put("race", fmt.Sprintf("https://x.com/%d", i), 0); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, ErrExists) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || s.Len() != 1 {
		t.Fatalf("vencedores = %d, Len = %d (esperado 1 e 1)", wins.Load(), s.Len())
	}
}

func TestConcurrentMixedOperationsWithCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.jsonl")
	clk := testclock.New()
	s := fileStore(t, path, clk)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := fmt.Sprintf("w%d-%d", w, i)
				s.Put(id, "https://x.com/"+id, 0)
				s.Visit(id)
				if i%3 == 0 {
					s.Delete(id)
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() { // compactações no meio da tempestade
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if err := s.Compact(); err != nil {
				t.Error(err)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	want := s.Len()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := fileStore(t, path, clk)
	defer s2.Close()
	if s2.Len() != want {
		t.Fatalf("após reabrir: %d links, esperado %d", s2.Len(), want)
	}
}

// ------------------------------------------------------------ persistência
func TestPersistenceRoundTripKeepsClicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "links.jsonl") // cria o diretório
	clk := testclock.New()
	s := fileStore(t, path, clk)
	s.Put("keep", "https://keep.com", 0)
	s.Put("gone", "https://gone.com", 0)
	s.Put("ttl", "https://ttl.com", time.Hour)
	for i := 0; i < 7; i++ {
		s.Visit("keep")
	}
	s.Delete("gone")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := fileStore(t, path, clk)
	defer s2.Close()
	if s2.Len() != 2 {
		t.Fatalf("Len = %d, esperado 2", s2.Len())
	}
	keep, err := s2.Lookup("keep")
	if err != nil || keep.Clicks != 7 || keep.LastClickAt == nil {
		t.Fatalf("keep = %+v, %v", keep, err)
	}
	if _, err := s2.Lookup("gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("'gone' deveria ter sumido: %v", err)
	}
	ttl, _ := s2.Lookup("ttl")
	if ttl.ExpiresAt == nil || !ttl.ExpiresAt.Equal(clk.Now().Add(time.Hour)) {
		t.Fatalf("expiração perdida: %+v", ttl)
	}
}

func TestCrashWithoutCloseKeepsLinksButLosesRecentClicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.jsonl")
	clk := testclock.New()
	s := fileStore(t, path, clk)
	s.Put("a", "https://a.com", 0)
	s.Put("b", "https://b.com", 0)
	s.Delete("b")
	s.Visit("a")
	s.log.close() // simula queda: sem compactação final

	s2 := fileStore(t, path, clk)
	defer s2.Close()
	if s2.Len() != 1 {
		t.Fatalf("Len = %d, esperado 1", s2.Len())
	}
	a, err := s2.Lookup("a")
	if err != nil || a.Clicks != 0 {
		t.Fatalf("a = %+v, %v (cliques desde a última compactação são perdidos, por projeto)", a, err)
	}
}

func TestTruncatedLastLineIsToleratedAndDoesNotCorruptNextRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.jsonl")
	good := `{"op":"put","id":"ok","url":"https://ok.com","created":1}` + "\n"
	partial := `{"op":"put","id":"half","url":"https://ha` // sem '\n' no fim
	if err := os.WriteFile(path, []byte(good+partial), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := testclock.New()
	s := fileStore(t, path, clk)
	if s.Replay.Records != 1 || s.Replay.Skipped != 1 {
		t.Fatalf("Replay = %+v, esperado 1 aplicado e 1 ignorado", s.Replay)
	}
	if _, err := s.Put("new", "https://new.com", 0); err != nil {
		t.Fatal(err)
	}
	s.log.close() // sem compactar: o log em disco é o que importa aqui

	s2 := fileStore(t, path, clk)
	defer s2.Close()
	for _, id := range []string{"ok", "new"} {
		if _, err := s2.Lookup(id); err != nil {
			t.Errorf("%q deveria existir após reabrir: %v", id, err)
		}
	}
	if _, err := s2.Lookup("half"); !errors.Is(err, ErrNotFound) {
		t.Errorf("registro quebrado não deveria existir: %v", err)
	}
}

func TestGarbageLinesAreSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.jsonl")
	content := "isto não é json\n" +
		`{"op":"put","id":"a","url":"https://a.com","created":1}` + "\n" +
		`{"op":"banana","id":"x"}` + "\n" +
		`{"op":"put","url":"sem id"}` + "\n" +
		"\n"
	os.WriteFile(path, []byte(content), 0o644)
	s := fileStore(t, path, testclock.New())
	defer s.Close()
	if s.Replay.Records != 1 || s.Replay.Skipped != 3 || s.Len() != 1 {
		t.Fatalf("Replay = %+v, Len = %d", s.Replay, s.Len())
	}
}

func TestCompactionShrinksLogAndKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.jsonl")
	clk := testclock.New()
	s := fileStore(t, path, clk)
	for i := 0; i < 200; i++ {
		s.Put(fmt.Sprintf("id%03d", i), "https://example.com/"+strings.Repeat("x", 40), 0)
	}
	for i := 0; i < 190; i++ {
		s.Delete(fmt.Sprintf("id%03d", i))
	}
	before, _ := os.Stat(path)
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size()/5 {
		t.Fatalf("log não encolheu o bastante: %d -> %d bytes", before.Size(), after.Size())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("arquivo temporário deveria ter sido removido (renomeado)")
	}
	// continua aceitando escritas depois de compactar
	if _, err := s.Put("after", "https://after.com", 0); err != nil {
		t.Fatalf("Put após compactar: %v", err)
	}
	s.Close()
	s2 := fileStore(t, path, clk)
	defer s2.Close()
	if s2.Len() != 11 {
		t.Fatalf("Len = %d, esperado 11", s2.Len())
	}
}

func TestCompactionDropsExpiredLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.jsonl")
	clk := testclock.New()
	s := fileStore(t, path, clk)
	s.Put("old", "https://old.com", time.Minute)
	s.Put("new", "https://new.com", 0)
	clk.Advance(time.Hour)
	s.Compact()
	if s.Len() != 1 {
		t.Fatalf("Len = %d, esperado 1", s.Len())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "old.com") {
		t.Fatal("link expirado continua no arquivo")
	}
	s.Close()
}

func TestCloseIsIdempotentAndBlocksWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.jsonl")
	s := fileStore(t, path, testclock.New())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("segundo Close: %v", err)
	}
	if _, err := s.Put("x", "https://x.com", 0); err == nil {
		t.Fatal("Put após Close deveria falhar")
	}
}

// ------------------------------------------------------------ benchmarks
func BenchmarkVisitParallel(b *testing.B) {
	s, _ := Open(Options{})
	for i := 0; i < 1000; i++ {
		s.Put(fmt.Sprintf("id%d", i), "https://example.com", 0)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Visit(fmt.Sprintf("id%d", i%1000))
			i++
		}
	})
}

func BenchmarkPutMemory(b *testing.B) {
	s, _ := Open(Options{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Put(fmt.Sprintf("id%d", i), "https://example.com", 0)
	}
}
