package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/thyagodev021/encurtador-go/internal/shortid"
)

type ctxKey string

const requestIDKey ctxKey = "request-id"

// statusRecorder captura status e tamanho da resposta para log e métricas.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap permite que http.ResponseController alcance o ResponseWriter original.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// requestID garante um X-Request-ID em toda requisição (reaproveita o do cliente, se válido).
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id, _ = shortid.New(12)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// recoverer transforma panics em 500 sem derrubar o servidor.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.cfg.Logger.Error("panic na requisição", "panic", rec,
					"path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "erro interno")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// instrument registra log estruturado e métricas de cada requisição.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		d := time.Since(start)
		s.metrics.observe(r.Method, rec.status, d)

		id, _ := r.Context().Value(requestIDKey).(string)
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		s.cfg.Logger.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method), slog.String("path", r.URL.Path),
			slog.Int("status", rec.status), slog.Duration("duration", d),
			slog.Int("bytes", rec.bytes), slog.String("ip", clientIP(r, s.cfg.TrustProxy)),
			slog.String("request_id", id))
	})
}

// clientIP devolve o IP do cliente. X-Forwarded-For só é confiável atrás de um proxy seu.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
