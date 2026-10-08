package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thyagodev021/encurtador-go/internal/ratelimit"
	"github.com/thyagodev021/encurtador-go/internal/store"
	"github.com/thyagodev021/encurtador-go/internal/testclock"
)

type env struct {
	ts    *httptest.Server
	store *store.Store
	clock *testclock.Clock
	http  *http.Client
}

// newEnv sobe um servidor de verdade (porta aleatória). O cliente NÃO segue redirects,
// para podermos inspecionar o 302 e o cabeçalho Location.
func newEnv(t *testing.T, limiter func(*testclock.Clock) *ratelimit.Limiter) *env {
	t.Helper()
	clk := testclock.New()
	st, err := store.Open(store.Options{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "http://short.test", Store: st}
	if limiter != nil {
		cfg.Limiter = limiter(clk)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{ts: ts, store: st, clock: clk, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (e *env) do(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) create(t *testing.T, body string) linkResp {
	t.Helper()
	resp, b := e.do(t, "POST", "/api/links", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("criar: status %d, corpo %s", resp.StatusCode, b)
	}
	var out linkResp
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type linkResp struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	ShortURL  string `json:"short_url"`
	Clicks    int64  `json:"clicks"`
	ExpiresAt string `json:"expires_at"`
}

func errorOf(t *testing.T, b []byte) string {
	t.Helper()
	var e struct{ Error string }
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("resposta não é um JSON de erro: %s", b)
	}
	return e.Error
}

func TestCreateRedirectAndStatsFlow(t *testing.T) {
	e := newEnv(t, nil)
	created := e.create(t, `{"url":"https://example.com/pagina?x=1"}`)
	if len(created.ID) != 7 || created.ShortURL != "http://short.test/"+created.ID {
		t.Fatalf("resposta inesperada: %+v", created)
	}

	for i := 0; i < 3; i++ {
		resp, _ := e.do(t, "GET", "/"+created.ID, "")
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("redirect: status %d", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "https://example.com/pagina?x=1" {
			t.Fatalf("Location = %q", loc)
		}
	}
	resp, b := e.do(t, "GET", "/api/links/"+created.ID, "")
	var stats linkResp
	json.Unmarshal(b, &stats)
	if resp.StatusCode != 200 || stats.Clicks != 3 {
		t.Fatalf("stats: status %d, cliques %d", resp.StatusCode, stats.Clicks)
	}
}

func TestCustomAlias(t *testing.T) {
	e := newEnv(t, nil)
	c := e.create(t, `{"url":"https://example.com","alias":"meu-link_1"}`)
	if c.ID != "meu-link_1" {
		t.Fatalf("ID = %q", c.ID)
	}
	resp, b := e.do(t, "POST", "/api/links", `{"url":"https://other.com","alias":"meu-link_1"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("alias repetido: status %d (%s)", resp.StatusCode, b)
	}
}

func TestValidationErrors(t *testing.T) {
	e := newEnv(t, nil)
	long := `{"url":"https://example.com/` + strings.Repeat("a", 2100) + `"}`
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"sem url", `{}`, "obrigatório", 400},
		{"javascript", `{"url":"javascript:alert(1)"}`, "http://", 400},
		{"data uri", `{"url":"data:text/html;base64,PHNjcmlwdD4="}`, "http://", 400},
		{"ftp", `{"url":"ftp://x.com/f"}`, "http://", 400},
		{"sem host", `{"url":"https://"}`, "host", 400},
		{"credenciais", `{"url":"https://admin:123@x.com"}`, "usuário/senha", 400},
		{"próprio serviço", `{"url":"http://short.test/abc"}`, "próprio serviço", 400},
		{"url longa", long, "caracteres", 400},
		{"alias curto", `{"url":"https://x.com","alias":"ab"}`, "alias", 400},
		{"alias inválido", `{"url":"https://x.com","alias":"a b!"}`, "alias", 400},
		{"alias reservado", `{"url":"https://x.com","alias":"API"}`, "reservado", 400},
		{"ttl negativo", `{"url":"https://x.com","ttl_seconds":-5}`, "ttl_seconds", 400},
		{"ttl enorme", `{"url":"https://x.com","ttl_seconds":99999999999}`, "ttl_seconds", 400},
		{"json quebrado", `{"url":`, "JSON", 400},
		{"campo desconhecido", `{"url":"https://x.com","foo":1}`, "JSON", 400},
		{"corpo gigante", `{"url":"` + strings.Repeat("a", 9000) + `"}`, "grande demais", 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, b := e.do(t, "POST", "/api/links", c.body)
			if resp.StatusCode != c.status || !strings.Contains(errorOf(t, b), c.want) {
				t.Fatalf("status %d, erro %q (esperado %d contendo %q)", resp.StatusCode, b, c.status, c.want)
			}
		})
	}
	if e.store.Len() != 0 {
		t.Fatalf("nenhuma requisição inválida deveria criar link; Len = %d", e.store.Len())
	}
}

func TestNotFoundAndInvalidIDs(t *testing.T) {
	e := newEnv(t, nil)
	for _, path := range []string{"/naoexiste", "/a%20b", "/" + strings.Repeat("x", 40), "/api/links/naoexiste", "/a/b/c"} {
		resp, b := e.do(t, "GET", path, "")
		if resp.StatusCode != 404 {
			t.Errorf("GET %s: status %d, esperado 404", path, resp.StatusCode)
		}
		errorOf(t, b) // todo 404 também é JSON
	}
}

func TestExpiredLinkReturns410(t *testing.T) {
	e := newEnv(t, nil)
	c := e.create(t, `{"url":"https://example.com","ttl_seconds":60}`)
	if c.ExpiresAt == "" {
		t.Fatal("expires_at deveria vir preenchido")
	}
	if resp, _ := e.do(t, "GET", "/"+c.ID, ""); resp.StatusCode != 302 {
		t.Fatalf("antes de expirar: %d", resp.StatusCode)
	}
	e.clock.Advance(61 * time.Second)
	for _, path := range []string{"/" + c.ID, "/api/links/" + c.ID} {
		if resp, _ := e.do(t, "GET", path, ""); resp.StatusCode != http.StatusGone {
			t.Fatalf("GET %s após expirar: %d, esperado 410", path, resp.StatusCode)
		}
	}
}

func TestHeadDoesNotCountClicks(t *testing.T) {
	e := newEnv(t, nil)
	c := e.create(t, `{"url":"https://example.com"}`)
	resp, _ := e.do(t, "HEAD", "/"+c.ID, "")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "https://example.com" {
		t.Fatalf("HEAD: %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	info, _ := e.store.Lookup(c.ID)
	if info.Clicks != 0 {
		t.Fatalf("HEAD contou clique: %d", info.Clicks)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t, nil)
	c := e.create(t, `{"url":"https://example.com"}`)
	if resp, _ := e.do(t, "DELETE", "/api/links/"+c.ID, ""); resp.StatusCode != 204 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/"+c.ID, ""); resp.StatusCode != 404 {
		t.Fatalf("após apagar: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/links/"+c.ID, ""); resp.StatusCode != 404 {
		t.Fatalf("apagar de novo: %d", resp.StatusCode)
	}
}

func TestListTopLinks(t *testing.T) {
	e := newEnv(t, nil)
	a := e.create(t, `{"url":"https://a.com","alias":"aaa"}`)
	b := e.create(t, `{"url":"https://b.com","alias":"bbb"}`)
	for i := 0; i < 4; i++ {
		e.do(t, "GET", "/"+b.ID, "")
	}
	e.do(t, "GET", "/"+a.ID, "")

	_, body := e.do(t, "GET", "/api/links?limit=1", "")
	var out struct{ Links []linkResp }
	json.Unmarshal(body, &out)
	if len(out.Links) != 1 || out.Links[0].ID != "bbb" || out.Links[0].Clicks != 4 {
		t.Fatalf("top = %+v", out.Links)
	}
	for _, bad := range []string{"0", "101", "abc"} {
		if resp, _ := e.do(t, "GET", "/api/links?limit="+bad, ""); resp.StatusCode != 400 {
			t.Errorf("limit=%s: status %d, esperado 400", bad, resp.StatusCode)
		}
	}
}

func TestRateLimitOnWritesOnly(t *testing.T) {
	e := newEnv(t, func(c *testclock.Clock) *ratelimit.Limiter { return ratelimit.New(1, 2, c.Now) })
	for i := 0; i < 2; i++ {
		e.create(t, fmt.Sprintf(`{"url":"https://x.com/%d"}`, i))
	}
	resp, b := e.do(t, "POST", "/api/links", `{"url":"https://x.com/3"}`)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("3ª criação: status %d, Retry-After %q, corpo %s", resp.StatusCode, resp.Header.Get("Retry-After"), b)
	}
	// leituras e redirects NÃO passam pelo limitador
	c := e.store.Top(1)[0]
	for i := 0; i < 20; i++ {
		if r, _ := e.do(t, "GET", "/"+c.ID, ""); r.StatusCode != 302 {
			t.Fatalf("redirect %d bloqueado: %d", i, r.StatusCode)
		}
	}
	e.clock.Advance(2 * time.Second) // repõe tokens
	e.create(t, `{"url":"https://x.com/depois"}`)
}

func TestHealthAndMetrics(t *testing.T) {
	e := newEnv(t, nil)
	c := e.create(t, `{"url":"https://example.com"}`)
	e.do(t, "GET", "/"+c.ID, "")
	e.do(t, "GET", "/naoexiste", "")

	resp, b := e.do(t, "GET", "/healthz", "")
	var h struct {
		Status string
		Links  int
	}
	json.Unmarshal(b, &h)
	if resp.StatusCode != 200 || h.Status != "ok" || h.Links != 1 {
		t.Fatalf("healthz = %d %s", resp.StatusCode, b)
	}

	_, m := e.do(t, "GET", "/metrics", "")
	text := string(m)
	for _, want := range []string{
		"shortener_links 1",
		"shortener_links_created_total 1",
		"shortener_redirects_total 1",
		`http_requests_total{method="POST",code="201"} 1`,
		`http_requests_total{method="GET",code="302"} 1`,
		`http_requests_total{method="GET",code="404"} 1`,
		`http_request_duration_seconds_bucket{le="+Inf"}`,
		"http_request_duration_seconds_count",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("métricas sem %q\n%s", want, text)
		}
	}
}

func TestRequestIDHeader(t *testing.T) {
	e := newEnv(t, nil)
	resp, _ := e.do(t, "GET", "/healthz", "")
	if len(resp.Header.Get("X-Request-ID")) != 12 {
		t.Fatalf("X-Request-ID gerado = %q", resp.Header.Get("X-Request-ID"))
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/healthz", nil)
	req.Header.Set("X-Request-ID", "meu-id-123")
	r2, _ := e.http.Do(req)
	r2.Body.Close()
	if r2.Header.Get("X-Request-ID") != "meu-id-123" {
		t.Fatalf("deveria reaproveitar o ID do cliente: %q", r2.Header.Get("X-Request-ID"))
	}
}

func TestPanicBecomes500AndServerSurvives(t *testing.T) {
	st, _ := store.Open(store.Options{})
	srv, _ := New(Config{BaseURL: "http://short.test", Store: st})
	h := srv.requestID(srv.recoverer(srv.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("kaboom")
		}
		w.Write([]byte("ok"))
	}))))
	ts := httptest.NewServer(h)
	defer ts.Close()
	r1, _ := http.Get(ts.URL + "/boom")
	r1.Body.Close()
	r2, _ := http.Get(ts.URL + "/fine")
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r1.StatusCode != 500 || r2.StatusCode != 200 || string(b) != "ok" {
		t.Fatalf("panic: %d, depois: %d %q", r1.StatusCode, r2.StatusCode, b)
	}
}

func TestClientIPAndProxyTrust(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", " 203.0.113.9 , 10.0.0.1")
	if ip := clientIP(req, false); ip != "10.0.0.1" {
		t.Errorf("sem confiar no proxy: %q", ip)
	}
	if ip := clientIP(req, true); ip != "203.0.113.9" {
		t.Errorf("confiando no proxy: %q", ip)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	st, _ := store.Open(store.Options{})
	for _, bad := range []string{"", "ftp://x", "localhost:8080", "http://"} {
		if _, err := New(Config{BaseURL: bad, Store: st}); err == nil {
			t.Errorf("BaseURL %q deveria ser rejeitada", bad)
		}
	}
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Error("sem Store deveria falhar")
	}
}

func TestConcurrentCreatesAndRedirects(t *testing.T) {
	e := newEnv(t, nil)
	var wg sync.WaitGroup
	ids := make(chan string, 200)
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				req, _ := http.NewRequest("POST", e.ts.URL+"/api/links",
					bytes.NewBufferString(fmt.Sprintf(`{"url":"https://x.com/%d/%d"}`, w, i)))
				resp, err := e.http.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				var c linkResp
				json.NewDecoder(resp.Body).Decode(&c)
				resp.Body.Close()
				ids <- c.ID
				if r, err := e.http.Get(e.ts.URL + "/" + c.ID); err == nil {
					r.Body.Close()
				}
			}
		}(w)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("ID vazio ou duplicado: %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 200 || e.store.Len() != 200 {
		t.Fatalf("links = %d, Len = %d, esperado 200", len(seen), e.store.Len())
	}
}

func BenchmarkRedirect(b *testing.B) {
	st, _ := store.Open(store.Options{})
	st.Put("bench", "https://example.com", 0)
	srv, _ := New(Config{BaseURL: "http://short.test", Store: st})
	h := srv.Handler()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/bench", nil))
		}
	})
}
