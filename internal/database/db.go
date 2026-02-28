package database

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"go-polyglot-persistence/internal/metrics"
	"go-polyglot-persistence/internal/models"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
)

// Operation timeouts.
// These cap how long a single DB call can hold a connection / wait on a lock.
// They are intentionally tighter than the HTTP WriteTimeout so the handler
// can return a clean 500 before the client's TCP connection times out.
const (
	readTimeout    = 5 * time.Second
	writeTimeout   = 5 * time.Second
	refreshTimeout = 5 * time.Minute // REFRESH MATERIALIZED VIEW can be slow
)

type DailySale struct {
	Date         string  `json:"date"`
	TotalRevenue float64 `json:"total_revenue"`
}

type DB struct {
	Conn *sql.DB
}

// Connect opens and verifies a Postgres connection.
func Connect(connStr string) (*DB, error) {
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		return nil, err
	}
	slog.Info("postgres connected")
	return &DB{Conn: conn}, nil
}

// GetOrderByID fetches a single order by its UUID.
// Returns sql.ErrNoRows when the ID does not exist — callers must distinguish
// this from other errors to return the correct HTTP status code.
func (db *DB) GetOrderByID(ctx context.Context, id string) (*models.Order, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	var o models.Order
	err := db.Conn.QueryRowContext(ctx,
		"SELECT id, product_name, amount, created_at FROM orders WHERE id = $1",
		id,
	).Scan(&o.ID, &o.ProductName, &o.Amount, &o.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// GetSales queries the daily_sales_mv materialized view.
func (db *DB) GetSales(ctx context.Context) ([]DailySale, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	timer := prometheus.NewTimer(metrics.DBQueryDuration.WithLabelValues("read_dashboard"))
	defer timer.ObserveDuration()

	rows, err := db.Conn.QueryContext(ctx,
		"SELECT sale_date, total_revenue FROM daily_sales_mv ORDER BY sale_date DESC LIMIT 30",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sales []DailySale
	for rows.Next() {
		var s DailySale
		if err := rows.Scan(&s.Date, &s.TotalRevenue); err != nil {
			slog.Error("scan failed", "op", "get_sales", "error", err)
			continue
		}
		sales = append(sales, s)
	}
	return sales, rows.Err()
}

// RefreshMaterializedView triggers REFRESH MATERIALIZED VIEW CONCURRENTLY.
// CONCURRENTLY means reads are not blocked, but a unique index is required.
// This operation can be slow on large tables — it gets its own long timeout,
// separate from the HTTP request context, so an admin trigger does not race
// against the server's WriteTimeout.
func (db *DB) RefreshMaterializedView(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	timer := prometheus.NewTimer(metrics.DBQueryDuration.WithLabelValues("refresh_mv"))
	defer timer.ObserveDuration()

	_, err := db.Conn.ExecContext(ctx, "REFRESH MATERIALIZED VIEW CONCURRENTLY daily_sales_mv")
	return err
}

// InsertOrder inserts a single order row.
func (db *DB) InsertOrder(ctx context.Context, productName string, amount float64) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	_, err := db.Conn.ExecContext(ctx,
		"INSERT INTO orders (product_name, amount, created_at) VALUES ($1, $2, NOW())",
		productName, amount,
	)
	return err
}

// InsertOrderIdempotent inserts an order by its pre-assigned UUID.
// ON CONFLICT DO NOTHING makes retries safe — replaying the same message
// from RabbitMQ will not create duplicate rows.
func (db *DB) InsertOrderIdempotent(ctx context.Context, o *models.Order) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	_, err := db.Conn.ExecContext(ctx,
		`INSERT INTO orders (id, product_name, amount, created_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (id) DO NOTHING`,
		o.ID, o.ProductName, o.Amount, o.CreatedAt,
	)
	return err
}

// ProcessBulkOrder inserts two items inside a single transaction.
// If item2 == "ERROR" the transaction is rolled back to demonstrate
// atomicity. The deferred Rollback is a no-op after a successful Commit.
func (db *DB) ProcessBulkOrder(ctx context.Context, item1, item2 string) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	tx, err := db.Conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err = tx.ExecContext(ctx,
		"INSERT INTO orders (product_name, amount, created_at) VALUES ($1, $2, NOW())",
		item1, 100.00,
	); err != nil {
		return err
	}
	slog.Info("bulk order step 1 ok", "item", item1)

	if item2 == "ERROR" {
		slog.Warn("bulk order simulating failure", "item", item2)
		return errors.New("simulated failure at step 2")
	}

	if _, err = tx.ExecContext(ctx,
		"INSERT INTO orders (product_name, amount, created_at) VALUES ($1, $2, NOW())",
		item2, 50.00,
	); err != nil {
		return err
	}
	slog.Info("bulk order step 2 ok", "item", item2)

	if err = tx.Commit(); err != nil {
		return err
	}
	slog.Info("bulk order committed")
	return nil
}
