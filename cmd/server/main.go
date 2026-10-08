// Comando server: encurtador de links com API REST.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/thyagodev021/encurtador-go/internal/ratelimit"
	"github.com/thyagodev021/encurtador-go/internal/server"
	"github.com/thyagodev021/encurtador-go/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run() error {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "endereço de escuta")
	baseURL := flag.String("base-url", envOr("BASE_URL", "http://localhost:8080"), "URL pública usada nos links curtos")
	data := flag.String("data", envOr("DATA_FILE", "data/links.jsonl"), "arquivo de dados (vazio = só memória)")
	rate := flag.Float64("rate", 5, "requisições de escrita por segundo, por IP (0 desliga)")
	burst := flag.Int("burst", 10, "rajada máxima do rate limit")
	idLen := flag.Int("id-length", 7, "tamanho dos IDs gerados")
	compactEvery := flag.Duration("compact-every", 10*time.Minute, "intervalo entre compactações do log")
	sweepEvery := flag.Duration("sweep-every", time.Minute, "intervalo da limpeza de links expirados")
	trustProxy := flag.Bool("trust-proxy", false, "confiar no X-Forwarded-For (use só atrás de um proxy seu)")
	syncWrites := flag.Bool("sync", false, "fsync a cada escrita (mais seguro, mais lento)")
	logFormat := flag.String("log-format", envOr("LOG_FORMAT", "text"), "formato do log: text ou json")
	flag.Parse()

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	logger := slog.New(handler)

	st, err := store.Open(store.Options{Path: *data, SyncWrites: *syncWrites})
	if err != nil {
		return fmt.Errorf("abrindo o armazenamento: %w", err)
	}
	logger.Info("armazenamento pronto", "arquivo", *data, "links", st.Len(),
		"registros_lidos", st.Replay.Records, "ignorados", st.Replay.Skipped)

	var limiter *ratelimit.Limiter
	if *rate > 0 {
		limiter = ratelimit.New(*rate, *burst, nil)
	}
	srv, err := server.New(server.Config{BaseURL: *baseURL, Store: st, Limiter: limiter,
		Logger: logger, TrustProxy: *trustProxy, IDLength: *idLen})
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var workers sync.WaitGroup
	every := func(d time.Duration, name string, fn func()) {
		if d <= 0 {
			return
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fn()
				}
			}
		}()
	}
	every(*sweepEvery, "sweep", func() {
		if n := st.SweepExpired(); n > 0 {
			logger.Info("links expirados removidos", "quantidade", n)
		}
		if limiter != nil {
			limiter.Sweep(10 * time.Minute)
		}
	})
	every(*compactEvery, "compact", func() {
		if err := st.Compact(); err != nil {
			logger.Error("falha ao compactar o log", "err", err)
		}
	})

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	logger.Info("servidor no ar", "addr", *addr, "base_url", *baseURL)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("encerrando (SIGINT/SIGTERM)...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("falha no shutdown", "err", err)
	}
	stop()
	workers.Wait()
	if err := st.Close(); err != nil { // compacta e grava os cliques
		return fmt.Errorf("fechando o armazenamento: %w", err)
	}
	logger.Info("até logo!")
	return nil
}
