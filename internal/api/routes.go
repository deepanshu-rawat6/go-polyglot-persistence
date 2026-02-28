package api

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// RegisterRoutes attaches all application routes to mux.
// Keeping this separate from handlers.go means the full route surface
// is visible at a glance without scrolling through handler logic.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Orders
	mux.HandleFunc("POST /api/orders", h.CreateOrder)
	mux.HandleFunc("GET /api/orders/", h.GetOrder)

	// Search
	mux.HandleFunc("GET /api/search", h.SearchOrders)

	// Dashboard (materialized view)
	mux.HandleFunc("GET /api/dashboard/sales", h.GetSalesDashboard)

	// Admin
	mux.HandleFunc("POST /api/admin/refresh", h.RefreshMaterializedView)
	mux.HandleFunc("POST /api/bulk-orders", h.CreateBulkOrder)

	// Observability
	mux.Handle("GET /metrics", promhttp.Handler())
}
