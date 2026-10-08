// Package store guarda os links em memória (com locks fragmentados) e, opcionalmente,
// persiste tudo em um log append-only que é reaplicado ao iniciar.
//
// Ordem dos locks (sempre nesta ordem, para evitar deadlock):
//
//	persist  ->  shard.mu  ->  log.mu
package store

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNotFound: o ID não existe.
	ErrNotFound = errors.New("link não encontrado")
	// ErrExists: o ID já está em uso por um link ativo.
	ErrExists = errors.New("alias já está em uso")
	// ErrExpired: o link existia, mas passou da validade.
	ErrExpired = errors.New("link expirado")
)

const numShards = 16

// Link é um link encurtado. Os contadores são atômicos para que um redirecionamento
// (o caminho mais quente) precise só de um RLock.
type Link struct {
	ID        string
	URL       string
	CreatedAt time.Time
	ExpiresAt time.Time // zero = nunca expira

	clicks    atomic.Int64
	lastClick atomic.Int64 // unix nano; 0 = nunca clicado
}

func (l *Link) expired(now time.Time) bool {
	return !l.ExpiresAt.IsZero() && !now.Before(l.ExpiresAt)
}

// Info é uma cópia imutável de um Link, própria para virar JSON.
type Info struct {
	ID          string     `json:"id"`
	URL         string     `json:"url"`
	Clicks      int64      `json:"clicks"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastClickAt *time.Time `json:"last_click_at,omitempty"`
}

func (l *Link) info() Info {
	in := Info{ID: l.ID, URL: l.URL, Clicks: l.clicks.Load(), CreatedAt: l.CreatedAt}
	if !l.ExpiresAt.IsZero() {
		t := l.ExpiresAt
		in.ExpiresAt = &t
	}
	if ns := l.lastClick.Load(); ns != 0 {
		t := time.Unix(0, ns).UTC()
		in.LastClickAt = &t
	}
	return in
}

type shard struct {
	mu sync.RWMutex
	m  map[string]*Link
}

// Options configura o Store.
type Options struct {
	Path       string           // arquivo do log; vazio = só memória
	SyncWrites bool             // fsync a cada escrita (mais seguro, mais lento)
	Now        func() time.Time // relógio (nil = time.Now)
}

// ReplayStats resume a leitura do log ao abrir.
type ReplayStats struct {
	Records int // linhas aplicadas
	Skipped int // linhas corrompidas ou desconhecidas, ignoradas
}

// Store é seguro para uso concorrente.
type Store struct {
	shards  [numShards]shard
	persist sync.RWMutex // mutações: RLock; compactação: Lock
	log     *appendLog
	now     func() time.Time
	count   atomic.Int64
	closed  atomic.Bool

	Replay ReplayStats
}

func shardIndex(id string) uint32 {
	h := uint32(2166136261) // FNV-1a
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return h % numShards
}

func (s *Store) shardFor(id string) *shard { return &s.shards[shardIndex(id)] }

// Open cria um Store; se opt.Path != "", reaplica o log existente.
func Open(opt Options) (*Store, error) {
	s := &Store{now: opt.Now}
	if s.now == nil {
		s.now = time.Now
	}
	for i := range s.shards {
		s.shards[i].m = make(map[string]*Link)
	}
	if opt.Path == "" {
		return s, nil
	}
	stats, err := replayFile(opt.Path, s)
	if err != nil {
		return nil, err
	}
	s.Replay = stats
	l, err := openLog(opt.Path, opt.SyncWrites)
	if err != nil {
		return nil, err
	}
	s.log = l
	return s, nil
}

// Put cria um link. ttl <= 0 significa "não expira".
func (s *Store) Put(id, url string, ttl time.Duration) (Info, error) {
	now := s.now()
	l := &Link{ID: id, URL: url, CreatedAt: now}
	if ttl > 0 {
		l.ExpiresAt = now.Add(ttl)
	}

	s.persist.RLock()
	defer s.persist.RUnlock()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	existing, found := sh.m[id]
	if found && !existing.expired(now) {
		return Info{}, ErrExists
	}
	if s.log != nil {
		if err := s.log.append(recordFor(l)); err != nil {
			return Info{}, err
		}
	}
	sh.m[id] = l
	if !found {
		s.count.Add(1)
	}
	return l.info(), nil
}

// Visit registra um clique e devolve a URL de destino. É o caminho quente: só RLock + atômicos.
func (s *Store) Visit(id string) (string, error) {
	sh := s.shardFor(id)
	sh.mu.RLock()
	l, ok := sh.m[id]
	sh.mu.RUnlock()
	if !ok {
		return "", ErrNotFound
	}
	now := s.now()
	if l.expired(now) {
		return "", ErrExpired
	}
	l.clicks.Add(1)
	l.lastClick.Store(now.UnixNano())
	return l.URL, nil
}

// Lookup devolve os dados do link sem contar clique.
func (s *Store) Lookup(id string) (Info, error) {
	sh := s.shardFor(id)
	sh.mu.RLock()
	l, ok := sh.m[id]
	sh.mu.RUnlock()
	if !ok {
		return Info{}, ErrNotFound
	}
	if l.expired(s.now()) {
		return Info{}, ErrExpired
	}
	return l.info(), nil
}

// Delete remove um link.
func (s *Store) Delete(id string) error {
	s.persist.RLock()
	defer s.persist.RUnlock()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, ok := sh.m[id]; !ok {
		return ErrNotFound
	}
	if s.log != nil {
		if err := s.log.append(record{Op: opDel, ID: id}); err != nil {
			return err
		}
	}
	delete(sh.m, id)
	s.count.Add(-1)
	return nil
}

// Len é o número de links guardados (inclui expirados ainda não varridos).
func (s *Store) Len() int { return int(s.count.Load()) }

// Top devolve até n links ativos, do mais clicado para o menos.
func (s *Store) Top(n int) []Info {
	now := s.now()
	var all []Info
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for _, l := range sh.m {
			if !l.expired(now) {
				all = append(all, l.info())
			}
		}
		sh.mu.RUnlock()
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Clicks != all[j].Clicks {
			return all[i].Clicks > all[j].Clicks
		}
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	if n >= 0 && len(all) > n {
		all = all[:n]
	}
	return all
}

// SweepExpired apaga os links expirados e devolve quantos foram removidos.
func (s *Store) SweepExpired() int {
	now := s.now()
	removed := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		var ids []string
		for id, l := range sh.m {
			if l.expired(now) {
				ids = append(ids, id)
			}
		}
		sh.mu.RUnlock()
		for _, id := range ids {
			if s.deleteIfExpired(id, now) {
				removed++
			}
		}
	}
	return removed
}

// deleteIfExpired reconfere sob o lock de escrita: o link pode ter sido recriado entre a coleta e a remoção.
func (s *Store) deleteIfExpired(id string, now time.Time) bool {
	s.persist.RLock()
	defer s.persist.RUnlock()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	l, ok := sh.m[id]
	if !ok || !l.expired(now) {
		return false
	}
	if s.log != nil {
		if err := s.log.append(record{Op: opDel, ID: id}); err != nil {
			return false
		}
	}
	delete(sh.m, id)
	s.count.Add(-1)
	return true
}

// Close compacta o log (gravando também os contadores de cliques) e o fecha.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if s.log == nil {
		return nil
	}
	err := s.compact()
	if cerr := s.log.close(); err == nil {
		err = cerr
	}
	return err
}
