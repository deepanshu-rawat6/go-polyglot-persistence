package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-polyglot-persistence/internal/database"
	"go-polyglot-persistence/internal/models"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Dependency interfaces
//
// Each interface captures exactly the methods this package needs.
// Callers (main, tests) inject the real implementations or fakes.
// ---------------------------------------------------------------------------

// OrderCache is the write-back cache contract.
type OrderCache interface {
	SetOrder(ctx context.Context, order *models.Order) error
	GetOrder(ctx context.Context, id string) (*models.Order, error)
}

// OrderQueue is the publish contract for the message broker.
type OrderQueue interface {
	PublishOrder(ctx context.Context, order *models.Order) error
}

// OrderSearch is the full-text search contract.
type OrderSearch interface {
	SearchOrders(ctx context.Context, term string) (json.RawMessage, error)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Handler holds every dependency the HTTP layer needs.
// All fields are interfaces — the real implementations are injected by main,
// fakes or mocks can be injected in tests.
type Handler struct {
	DB        *database.DB // owns SQL; stays concrete because it also drives the cron
	Cache     OrderCache
	Publisher OrderQueue
	Search    OrderSearch
}

// ---------------------------------------------------------------------------
// Orders
// ---------------------------------------------------------------------------

// CreateOrder — POST /api/orders
//
// Write-back path:
//  1. Assign UUID + timestamp.
//  2. Cache in Redis immediately so a GET can return before the worker runs.
//  3. Publish to RabbitMQ — worker persists to Postgres + ES asynchronously.
//  4. Return 202 Accepted; caller never waits for a DB write.
func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	var order models.Order
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	order.ID = uuid.New().String()
	order.CreatedAt = time.Now().UTC()
	ctx := r.Context()

	if err := h.Cache.SetOrder(ctx, &order); err != nil {
		// Non-fatal: the message still enters the queue and will be persisted.
		slog.Error("cache write failed",
			"component", "api",
			"order_id", order.ID,
			"error", err,
		)
	}

	if err := h.Publisher.PublishOrder(ctx, &order); err != nil {
		slog.Error("queue publish failed",
			"component", "api",
			"order_id", order.ID,
			"error", err,
		)
		http.Error(w, "failed to enqueue order", http.StatusInternalServerError)
		return
	}

	slog.Info("order accepted",
		"component", "api",
		"order_id", order.ID,
		"product", order.ProductName,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"status":   "processing",
		"order_id": order.ID,
	})
}

// GetOrder — GET /api/orders/{id}
//
// Read path:
//   - Redis HIT  → return instantly              (X-Cache: HIT)
//   - Redis MISS → Postgres lookup → back-fill   (X-Cache: MISS)
//   - sql.ErrNoRows → 404   (genuine not-found)
//   - any other DB error → 500  (infra failure, not a 404)
func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	orderID := strings.TrimPrefix(r.URL.Path, "/api/orders/")
	if orderID == "" {
		http.Error(w, "missing order ID", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	// Cache HIT
	if order, err := h.Cache.GetOrder(ctx, orderID); err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		json.NewEncoder(w).Encode(order)
		return
	}

	// Cache MISS → Postgres
	order, err := h.DB.GetOrderByID(ctx, orderID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("postgres read failed",
			"component", "api",
			"order_id", orderID,
			"error", err,
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	_ = h.Cache.SetOrder(ctx, order) // back-fill; failure is non-fatal

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	json.NewEncoder(w).Encode(order)
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// SearchOrders — GET /api/search?q={term}
//
// Proxies a full-text match on product_name to Elasticsearch.
func (h *Handler) SearchOrders(w http.ResponseWriter, r *http.Request) {
	term := r.URL.Query().Get("q")
	if term == "" {
		http.Error(w, "missing required query parameter: q", http.StatusBadRequest)
		return
	}

	result, err := h.Search.SearchOrders(r.Context(), term)
	if err != nil {
		slog.Error("elasticsearch search failed",
			"component", "api",
			"term", term,
			"error", err,
		)
		http.Error(w, "search engine error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

// GetSalesDashboard — GET /api/dashboard/sales
//
// Returns the last 30 days of pre-aggregated daily revenue from daily_sales_mv.
// Reads are fast: the GROUP BY runs at refresh time, not at query time.
func (h *Handler) GetSalesDashboard(w http.ResponseWriter, r *http.Request) {
	sales, err := h.DB.GetSales(r.Context())
	if err != nil {
		slog.Error("dashboard query failed",
			"component", "api",
			"client_ip", r.RemoteAddr,
			"error", err,
		)
		http.Error(w, "failed to fetch dashboard data", http.StatusInternalServerError)
		return
	}

	slog.Info("dashboard fetched", "component", "api", "records", len(sales))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sales)
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

// RefreshMaterializedView — POST /api/admin/refresh
//
// Manually triggers REFRESH MATERIALIZED VIEW CONCURRENTLY.
// The DB layer applies its own refreshTimeout (5 min) so this does not race
// against the HTTP server's WriteTimeout.
func (h *Handler) RefreshMaterializedView(w http.ResponseWriter, r *http.Request) {
	if err := h.DB.RefreshMaterializedView(r.Context()); err != nil {
		slog.Error("manual mv refresh failed", "component", "api", "error", err)
		http.Error(w, "failed to refresh view: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("materialized view refreshed", "component", "api", "trigger", "manual")
	w.Write([]byte("Materialized view refreshed successfully.\n"))
}

// CreateBulkOrder — POST /api/bulk-orders
//
// Inserts two items in a single transaction.
// Send {"item_1": "Laptop", "item_2": "ERROR"} to trigger a rollback.
func (h *Handler) CreateBulkOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Item1 string `json:"item_1"`
		Item2 string `json:"item_2"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.DB.ProcessBulkOrder(r.Context(), req.Item1, req.Item2); err != nil {
		slog.Error("bulk order failed", "component", "api", "error", err)
		http.Error(w, "transaction failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("Bulk order processed and committed.\n"))
}
