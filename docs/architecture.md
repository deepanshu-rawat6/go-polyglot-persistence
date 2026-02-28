# Architecture

## Data flows

### Write path — `POST /api/orders`

```
Client
  │  POST /api/orders {"product_name": "Laptop", "amount": 1299.99}
  ▼
API Service
  ├─ Assign UUID + UTC timestamp
  ├─ SET order:{id} → Redis  (write-back cache; TTL 24h)
  ├─ PUBLISH → RabbitMQ order_queue  (durable, persistent)
  └─ 202 Accepted  ← client unblocked here, no DB write yet

Worker Service  (consuming order_queue)
  ├─ INSERT INTO orders ... ON CONFLICT (id) DO NOTHING  → Postgres
  ├─ es.Index("orders", doc_id=order.id)                 → Elasticsearch
  └─ Ack message  (removed from queue permanently)
```

**Why 202 and not 201?**
The order is not yet in Postgres when the response is sent. 202 signals that the request was accepted for processing, not that it completed.

**Why write to Redis before the queue?**
So that a `GET /api/orders/{id}` immediately after a POST returns a cache HIT — even before the worker has run. Without this, the first read would always miss.

---

### Read path — `GET /api/orders/{id}`

```
Client
  │  GET /api/orders/{id}
  ▼
API Service
  ├─ GET order:{id} → Redis
  │     HIT  → respond (X-Cache: HIT)   ← fast path, no DB
  │     MISS ↓
  ├─ SELECT ... FROM orders WHERE id=$1 → Postgres
  ├─ SET order:{id} → Redis              (back-fill for next request)
  └─ respond (X-Cache: MISS)
```

**Error handling:**
- `sql.ErrNoRows` → `404 Not Found`
- Any other DB error → `500 Internal Server Error`

These are distinguished explicitly. Previously all DB errors mapped to 404, which hid infrastructure failures.

---

### Search path — `GET /api/search?q={term}`

```
Client
  │  GET /api/search?q=laptop
  ▼
API Service
  └─ ES match query on product_name → Elasticsearch
     └─ proxy raw response to client
```

Elasticsearch is used instead of Postgres full-text search because:
- **Inverted index** — sub-millisecond lookups at scale
- **Relevance scoring** — results ranked by match quality
- **No impact on Postgres** — search load is isolated

Postgres remains the source of truth. ES is a read-optimised projection, always populated by the worker after a successful Postgres insert.

---

### Dashboard path — `GET /api/dashboard/sales`

```
Client
  │  GET /api/dashboard/sales
  ▼
API Service
  └─ SELECT sale_date, total_revenue FROM daily_sales_mv
     LIMIT 30 → Postgres
```

`daily_sales_mv` is a materialized view that pre-aggregates `SUM(amount) GROUP BY date`. The `GROUP BY` runs at refresh time, not at query time — so this endpoint is a fast point-read regardless of how many rows are in the `orders` table.

The view is refreshed:
- **Automatically** — by the cron scheduler (default: `@hourly`, configurable via `MV_REFRESH_SCHEDULE`)
- **Manually** — via `POST /api/admin/refresh`

`REFRESH MATERIALIZED VIEW CONCURRENTLY` is used so live reads are never blocked during a refresh. This requires the unique index on `sale_date`.

---

## Idempotency

Both persistence steps in the worker are safe to replay:

| Step | Mechanism |
|------|-----------|
| Postgres insert | `ON CONFLICT (id) DO NOTHING` — replaying the same `order_id` is a no-op |
| Elasticsearch index | `WithDocumentID(order.ID)` — upsert semantics, same document replaces itself |

This matters because if Postgres succeeds but ES fails, the worker nacks the message and RabbitMQ redelivers it. Without idempotent writes, the retry would create a duplicate Postgres row.

---

## Package contracts

The `internal/api` package depends on three interfaces, not concrete types:

```go
type OrderCache interface {
    SetOrder(ctx context.Context, order *models.Order) error
    GetOrder(ctx context.Context, id string) (*models.Order, error)
}

type OrderQueue interface {
    PublishOrder(ctx context.Context, order *models.Order) error
}

type OrderSearch interface {
    SearchOrders(ctx context.Context, term string) (json.RawMessage, error)
}
```

This means:
- `internal/cache`, `internal/queue`, and `internal/search` can be swapped without touching `handlers.go`
- Unit tests can inject fakes without running any external service

`*database.DB` stays concrete in the handler because it also drives the cron job and needs to be passed to `worker.StartCronJobs`. All SQL lives inside `internal/database` — no raw queries outside that package.

---

## Context timeouts

Every database operation has an explicit timeout so a lock or slow query surfaces as a clean error rather than a hung goroutine:

| Operation | Timeout | Rationale |
|-----------|---------|-----------|
| `GetOrderByID` | 5s | Fast point read |
| `GetSales` | 5s | Pre-aggregated view read |
| `InsertOrder` | 5s | Single row write |
| `InsertOrderIdempotent` (worker) | 5s | Worker write; prevents goroutine leak on lock |
| `ProcessBulkOrder` | 5s | Transaction; capped to prevent cascading lock holds |
| `RefreshMaterializedView` | 5 min | Legitimately slow — but isolated from HTTP `WriteTimeout` |
| Worker per-message | 10s | Wraps Postgres + ES; if either hangs, message is nacked and requeued |

The refresh timeout is intentionally longer than the HTTP server's `WriteTimeout` (10s). The DB layer applies its own `context.WithTimeout` for the refresh, so the admin endpoint does not race against the server.
