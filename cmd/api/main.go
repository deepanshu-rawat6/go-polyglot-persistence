package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-polyglot-persistence/internal/api"
	"go-polyglot-persistence/internal/cache"
	"go-polyglot-persistence/internal/config"
	"go-polyglot-persistence/internal/database"
	"go-polyglot-persistence/internal/queue"
	"go-polyglot-persistence/internal/search"
	"go-polyglot-persistence/internal/worker"

	_ "github.com/lib/pq"
)

func main() {
	cfg := config.Load()

	// ── Infrastructure ─────────────────────────────────────────────────────────

	db, err := database.Connect(cfg.PostgresDSN)
	if err != nil {
		slog.Error("postgres connect failed", "error", err)
		os.Exit(1)
	}

	redisClient, err := cache.New(cfg.RedisAddr)
	if err != nil {
		slog.Error("redis connect failed", "error", err)
		os.Exit(1)
	}

	publisher, err := queue.NewPublisher(cfg.RabbitMQURL)
	if err != nil {
		slog.Error("rabbitmq connect failed", "error", err)
		os.Exit(1)
	}

	searchClient, err := search.New(cfg.ElasticsearchURL)
	if err != nil {
		slog.Error("elasticsearch init failed", "error", err)
		os.Exit(1)
	}

	// ── Background cron ────────────────────────────────────────────────────────

	cronScheduler, err := worker.StartCronJobs(db, cfg.MVRefreshSchedule)
	if err != nil {
		slog.Error("invalid cron schedule", "schedule", cfg.MVRefreshSchedule, "error", err)
		os.Exit(1)
	}

	// ── HTTP server ────────────────────────────────────────────────────────────

	h := &api.Handler{
		DB:        db,
		Cache:     redisClient,
		Publisher: publisher,
		Search:    searchClient,
	}

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:         ":" + cfg.APIPort,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("api started", "component", "api", "port", cfg.APIPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "component", "api", "error", err)
			os.Exit(1)
		}
	}()

	// ── Graceful shutdown ──────────────────────────────────────────────────────
	//
	// Shutdown order matters:
	//  1. Stop accepting new HTTP requests (srv.Shutdown) — in-flight requests finish.
	//  2. Stop the cron scheduler — waits for any running refresh to complete
	//     before returning, so db.Close() does not yank the connection mid-query.
	//  3. Close infrastructure clients (deferred below) in reverse init order.

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutdown signal received", "component", "api")

	httpCtx, httpCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer httpCancel()
	if err := srv.Shutdown(httpCtx); err != nil {
		slog.Error("http shutdown error", "component", "api", "error", err)
	}

	// cron.Stop() blocks until the currently-running job (if any) finishes.
	<-cronScheduler.Stop().Done()
	slog.Info("cron stopped", "component", "api")

	publisher.Close()
	redisClient.Close()
	db.Conn.Close()

	slog.Info("shutdown complete", "component", "api")
}
