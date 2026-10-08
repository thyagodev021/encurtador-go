// Package server expõe a API HTTP do encurtador.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thyagodev021/encurtador-go/internal/ratelimit"
	"github.com/thyagodev021/encurtador-go/internal/shortid"
	"github.com/thyagodev021/encurtador-go/internal/store"
)

const (
	maxURLLength   = 2048
	maxBodyBytes   = 8 << 10
	maxTTL         = 10 * 365 * 24 * time.Hour
	idRetries      = 8
	defaultIDLen   = 7
	defaultListLen = 10
	maxListLen     = 100
)

var (
	aliasRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)
	idRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	reserved = map[string]bool{"api": true, "healthz": true, "metrics": true}
)

// Config reúne as dependências do servidor.
type Config struct {
	BaseURL    string // ex.: https://enc.example.com (sem barra no fim)
	Store      *store.Store
	Limiter    *ratelimit.Limiter // nil = sem limite
	Logger     *slog.Logger       // nil = descarta os logs
	TrustProxy bool               // confiar no X-Forwarded-For
	IDLength   int                // tamanho dos IDs gerados (padrão 7)
}

// Server implementa http.Handler através de Handler().
type Server struct {
	cfg      Config
	metrics  *Metrics
	baseHost string
	started  time.Time
}

// New valida a configuração e cria o Server.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("server: Store é obrigatório")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("server: BaseURL inválida (use http://host:porta)")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.IDLength <= 0 {
		cfg.IDLength = defaultIDLen
	}
	return &Server{cfg: cfg, metrics: newMetrics(), baseHost: u.Host, started: time.Now()}, nil
}

// Handler monta as rotas e os middlewares.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/links", s.limited(s.handleCreate))
	mux.HandleFunc("GET /api/links", s.handleList)
	mux.HandleFunc("GET /api/links/{id}", s.handleStats)
	mux.HandleFunc("DELETE /api/links/{id}", s.limited(s.handleDelete))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /{id}", s.handleRedirect)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "rota não encontrada")
	})
	return s.requestID(s.recoverer(s.instrument(mux)))
}

// ------------------------------------------------------------ respostas
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type linkResponse struct {
	store.Info
	ShortURL string `json:"short_url"`
}

func (s *Server) respond(in store.Info) linkResponse {
	return linkResponse{Info: in, ShortURL: s.cfg.BaseURL + "/" + in.ID}
}

// limited aplica o rate limit por IP.
func (s *Server) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Limiter != nil {
			if ok, retry := s.cfg.Limiter.Allow(clientIP(r, s.cfg.TrustProxy)); !ok {
				s.metrics.rateLimited.Add(1)
				secs := int(math.Ceil(retry.Seconds()))
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				writeError(w, http.StatusTooManyRequests, "muitas requisições; tente novamente em instantes")
				return
			}
		}
		next(w, r)
	}
}

// ------------------------------------------------------------ validação
type createRequest struct {
	URL        string `json:"url"`
	Alias      string `json:"alias"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

// validateURL aceita apenas http(s) com host. Rejeita "javascript:", "data:", credenciais
// embutidas (user:senha@host, usado em phishing) e links para o próprio serviço (laços de redirect).
func (s *Server) validateURL(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return "", "o campo 'url' é obrigatório"
	case len(raw) > maxURLLength:
		return "", "a URL passa de " + strconv.Itoa(maxURLLength) + " caracteres"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "URL inválida"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "a URL deve começar com http:// ou https://"
	}
	if u.Hostname() == "" {
		return "", "a URL precisa ter um host"
	}
	if u.User != nil {
		return "", "URLs com usuário/senha não são permitidas"
	}
	if strings.EqualFold(u.Host, s.baseHost) {
		return "", "não é possível encurtar links deste próprio serviço"
	}
	return raw, ""
}

func validID(id string) bool { return idRe.MatchString(id) }

// ------------------------------------------------------------ handlers
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req createRequest
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "corpo da requisição grande demais")
			return
		}
		writeError(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	target, problem := s.validateURL(req.URL)
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}
	if req.TTLSeconds < 0 || time.Duration(req.TTLSeconds) > maxTTL/time.Second {
		writeError(w, http.StatusBadRequest, "ttl_seconds deve estar entre 0 e 315360000 (10 anos)")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second

	if req.Alias != "" {
		if !aliasRe.MatchString(req.Alias) {
			writeError(w, http.StatusBadRequest, "alias deve ter de 3 a 32 caracteres: letras, números, '-' ou '_'")
			return
		}
		if reserved[strings.ToLower(req.Alias)] {
			writeError(w, http.StatusBadRequest, "esse alias é reservado")
			return
		}
		in, err := s.cfg.Store.Put(req.Alias, target, ttl)
		s.finishCreate(w, in, err)
		return
	}

	for i := 0; i < idRetries; i++ {
		id, err := shortid.New(s.cfg.IDLength)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "erro ao gerar o ID")
			return
		}
		in, err := s.cfg.Store.Put(id, target, ttl)
		if errors.Is(err, store.ErrExists) {
			continue // colisão: tenta outro ID
		}
		s.finishCreate(w, in, err)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "não foi possível gerar um ID livre; tente novamente")
}

func (s *Server) finishCreate(w http.ResponseWriter, in store.Info, err error) {
	switch {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "esse alias já está em uso")
	case err != nil:
		s.cfg.Logger.Error("falha ao gravar link", "err", err)
		writeError(w, http.StatusInternalServerError, "erro ao salvar o link")
	default:
		s.metrics.created.Add(1)
		w.Header().Set("Location", "/api/links/"+in.ID)
		writeJSON(w, http.StatusCreated, s.respond(in))
	}
}

func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "link não encontrado")
		return
	}
	var target string
	var err error
	if r.Method == http.MethodHead {
		var in store.Info
		in, err = s.cfg.Store.Lookup(id) // HEAD não conta clique
		target = in.URL
	} else {
		target, err = s.cfg.Store.Visit(id)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "link não encontrado")
	case errors.Is(err, store.ErrExpired):
		writeError(w, http.StatusGone, "este link expirou")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "erro interno")
	default:
		if r.Method != http.MethodHead {
			s.metrics.redirects.Add(1)
		}
		http.Redirect(w, r, target, http.StatusFound) // 302: não cacheável, então cada acesso é contado
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	in, err := s.cfg.Store.Lookup(r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "link não encontrado")
	case errors.Is(err, store.ErrExpired):
		writeError(w, http.StatusGone, "este link expirou")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "erro interno")
	default:
		writeJSON(w, http.StatusOK, s.respond(in))
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := s.cfg.Store.Delete(r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "link não encontrado")
	case err != nil:
		s.cfg.Logger.Error("falha ao remover link", "err", err)
		writeError(w, http.StatusInternalServerError, "erro interno")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultListLen
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListLen {
			writeError(w, http.StatusBadRequest, "limit deve ser um número de 1 a "+strconv.Itoa(maxListLen))
			return
		}
		limit = n
	}
	top := s.cfg.Store.Top(limit)
	out := make([]linkResponse, 0, len(top))
	for _, in := range top {
		out = append(out, s.respond(in))
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"links":          s.cfg.Store.Len(),
		"uptime_seconds": int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.WritePrometheus(w, int64(s.cfg.Store.Len()))
}
