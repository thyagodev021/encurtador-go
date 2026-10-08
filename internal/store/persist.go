package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	opPut = "put"
	opDel = "del"
)

var errClosed = errors.New("store: log fechado")

// record é uma linha do log (JSON Lines).
type record struct {
	Op        string `json:"op"`
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Created   int64  `json:"created,omitempty"`    // unix nano
	Expires   int64  `json:"expires,omitempty"`    // unix nano; 0 = nunca
	Clicks    int64  `json:"clicks,omitempty"`     // só gravado na compactação
	LastClick int64  `json:"last_click,omitempty"` // idem
}

func recordFor(l *Link) record {
	r := record{Op: opPut, ID: l.ID, URL: l.URL, Created: l.CreatedAt.UnixNano(),
		Clicks: l.clicks.Load(), LastClick: l.lastClick.Load()}
	if !l.ExpiresAt.IsZero() {
		r.Expires = l.ExpiresAt.UnixNano()
	}
	return r
}

type appendLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	sync bool
}

func openLog(path string, syncWrites bool) (*appendLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	// Se a última linha ficou pela metade (queda de energia), termina-a com '\n'
	// para que o próximo registro não grude nela. A linha quebrada é ignorada no replay.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
	return &appendLog{path: path, f: f, sync: syncWrites}, nil
}

func (l *appendLog) append(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errClosed
	}
	if _, err := l.f.Write(b); err != nil {
		return err
	}
	if l.sync {
		return l.f.Sync()
	}
	return nil
}

func (l *appendLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// replayFile reaplica o log em s. Linhas inválidas são contadas e ignoradas.
func replayFile(path string, s *Store) (ReplayStats, error) {
	var stats ReplayStats
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return stats, nil
	}
	if err != nil {
		return stats, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r record
		if json.Unmarshal(line, &r) != nil || r.ID == "" {
			stats.Skipped++
			continue
		}
		switch r.Op {
		case opPut:
			s.restore(r)
		case opDel:
			s.removeRaw(r.ID)
		default:
			stats.Skipped++
			continue
		}
		stats.Records++
	}
	return stats, sc.Err()
}

func (s *Store) restore(r record) {
	l := &Link{ID: r.ID, URL: r.URL, CreatedAt: time.Unix(0, r.Created).UTC()}
	if r.Expires != 0 {
		l.ExpiresAt = time.Unix(0, r.Expires).UTC()
	}
	l.clicks.Store(r.Clicks)
	l.lastClick.Store(r.LastClick)
	sh := s.shardFor(r.ID)
	if _, exists := sh.m[r.ID]; !exists {
		s.count.Add(1)
	}
	sh.m[r.ID] = l
}

func (s *Store) removeRaw(id string) {
	sh := s.shardFor(id)
	if _, exists := sh.m[id]; exists {
		delete(sh.m, id)
		s.count.Add(-1)
	}
}

// Compact reescreve o log só com o estado atual (sem histórico, sem expirados),
// gravando também os cliques. A troca é atômica (arquivo temporário + rename).
func (s *Store) Compact() error {
	if s.closed.Load() {
		return nil
	}
	return s.compact()
}

func (s *Store) compact() error {
	if s.log == nil {
		return nil
	}
	s.persist.Lock() // bloqueia criações/remoções durante a compactação
	defer s.persist.Unlock()

	tmp := s.log.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	now := s.now()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for id, l := range sh.m {
			if l.expired(now) {
				delete(sh.m, id)
				s.count.Add(-1)
				continue
			}
			if err := enc.Encode(recordFor(l)); err != nil {
				sh.mu.Unlock()
				f.Close()
				os.Remove(tmp)
				return err
			}
		}
		sh.mu.Unlock()
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	if s.log.f != nil {
		s.log.f.Close() // fecha antes do rename (necessário no Windows)
		s.log.f = nil
	}
	if err := os.Rename(tmp, s.log.path); err != nil {
		return err
	}
	nf, err := os.OpenFile(s.log.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.log.f = nf
	return nil
}
