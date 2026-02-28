package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
		slog.Error("postgres connect failed", "component", "worker", "error", err)
		os.Exit(1)
	}

	searchClient, err := search.New(cfg.ElasticsearchURL)
	if err != nil {
		slog.Error("elasticsearch init failed", "component", "worker", "error", err)
		os.Exit(1)
	}

	consumer, err := queue.NewConsumer(cfg.RabbitMQURL)
	if err != nil {
		slog.Error("rabbitmq connect failed", "component", "worker", "error", err)
		os.Exit(1)
	}

	// ── Run ────────────────────────────────────────────────────────────────────
	//
	// ctx is cancelled on SIGINT/SIGTERM, which causes worker.Run to drain the
	// current in-flight message and return cleanly before we close connections.

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w := worker.New(db, searchClient, consumer)
	if err := w.Run(ctx); err != nil {
		slog.Error("worker error", "component", "worker", "error", err)
	}

	// ── Graceful shutdown ──────────────────────────────────────────────────────
	//
	// Run() has returned — the consume loop is done.
	// Close connections in reverse init order.

	consumer.Close()
	db.Conn.Close()

	slog.Info("worker stopped", "component", "worker")
}
