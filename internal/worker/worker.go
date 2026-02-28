package worker

import (
	"context"
	"log/slog"
	"time"

	"go-polyglot-persistence/internal/database"
	"go-polyglot-persistence/internal/queue"
	"go-polyglot-persistence/internal/search"
)

// perMessageTimeout caps how long a single Postgres + ES write can take.
// If Postgres holds a lock beyond this, the message is nacked and requeued
// rather than blocking the goroutine indefinitely.
const perMessageTimeout = 10 * time.Second

// Worker consumes orders from RabbitMQ and persists them to Postgres and ES.
type Worker struct {
	db       *database.DB
	search   *search.Client
	consumer *queue.Consumer
}

// New constructs a Worker. All dependencies are injected — no globals.
func New(db *database.DB, s *search.Client, c *queue.Consumer) *Worker {
	return &Worker{db: db, search: s, consumer: c}
}

// Run starts consuming messages and blocks until ctx is cancelled.
// On cancellation it drains any in-flight message before returning,
// so the caller's deferred Close() calls happen after the loop is clean.
func (w *Worker) Run(ctx context.Context) error {
	deliveries, err := w.consumer.Consume()
	if err != nil {
		return err
	}

	slog.Info("worker started", "component", "worker")

	for {
		select {
		case <-ctx.Done():
			slog.Info("worker shutting down", "component", "worker")
			return nil

		case delivery, ok := <-deliveries:
			if !ok {
				slog.Warn("delivery channel closed", "component", "worker")
				return nil
			}
			w.process(delivery)
		}
	}
}

// process handles a single delivery: write to Postgres, index in ES, then ack.
// Each step gets its own timeout so a lock or slow ES node cannot block forever.
func (w *Worker) process(d queue.Delivery) {
	order := d.Order

	ctx, cancel := context.WithTimeout(context.Background(), perMessageTimeout)
	defer cancel()

	// Step 1 — Postgres (source of truth, idempotent via ON CONFLICT DO NOTHING)
	if err := w.db.InsertOrderIdempotent(ctx, order); err != nil {
		slog.Error("postgres insert failed",
			"component", "worker",
			"order_id", order.ID,
			"error", err,
		)
		d.Nack()
		return
	}

	// Step 2 — Elasticsearch (search projection, idempotent via document ID upsert)
	if err := w.search.IndexOrder(ctx, order); err != nil {
		slog.Error("elasticsearch index failed",
			"component", "worker",
			"order_id", order.ID,
			"error", err,
		)
		// Postgres row exists; ON CONFLICT DO NOTHING handles the replay.
		d.Nack()
		return
	}

	// Step 3 — Ack: remove from queue only after both writes succeeded
	if err := d.Ack(); err != nil {
		slog.Error("ack failed", "component", "worker", "order_id", order.ID, "error", err)
		return
	}

	slog.Info("order processed",
		"component", "worker",
		"order_id", order.ID,
		"product", order.ProductName,
	)
}
