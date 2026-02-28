# Operations

## Configuration

All settings are read from environment variables. Defaults match docker-compose service names so the stack works out of the box with `docker compose up`.

| Variable              | Default                                         | Used by      |
|-----------------------|-------------------------------------------------|--------------|
| `POSTGRES_DSN`        | `user=postgres password=secret dbname=ecommerce sslmode=disable host=postgres` | api, worker |
| `REDIS_ADDR`          | `redis:6379`                                    | api          |
| `RABBITMQ_URL`        | `amqp://guest:guest@rabbitmq:5672/`             | api, worker  |
| `ELASTICSEARCH_URL`   | `http://elasticsearch:9200`                     | api, worker  |
| `API_PORT`            | `8080`                                          | api          |
| `MV_REFRESH_SCHEDULE` | `@hourly`                                       | api (cron)   |

`MV_REFRESH_SCHEDULE` accepts standard cron syntax (`0 * * * *`) or descriptors (`@hourly`, `@every 15m`).

---

## API reference

### Orders

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/orders` | Create an order. Returns `202 Accepted` immediately. |
| `GET`  | `/api/orders/{id}` | Fetch an order by ID. Check `X-Cache` header for `HIT`/`MISS`. |

### Search

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/search?q={term}` | Full-text search on `product_name` via Elasticsearch. |

### Dashboard

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/dashboard/sales` | Last 30 days of daily revenue from the materialized view. |

### Admin

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/refresh` | Manually trigger `REFRESH MATERIALIZED VIEW CONCURRENTLY`. |
| `POST` | `/api/bulk-orders` | Transactional two-item insert. Send `"item_2": "ERROR"` to trigger rollback. |

### Observability

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/metrics` | Prometheus scrape endpoint. |

---

## Example requests

```bash
# Create an order — returns 202 immediately, persisted async by the worker
curl -s -X POST http://localhost:8080/api/orders \
  -H "Content-Type: application/json" \
  -d '{"product_name": "Laptop", "amount": 1299.99}' | jq

# Read an order — X-Cache: HIT if Redis has it, MISS on fallback to Postgres
curl -si http://localhost:8080/api/orders/<id> | grep -E "X-Cache|{"

# Full-text search via Elasticsearch
curl -s "http://localhost:8080/api/search?q=laptop" | jq

# Sales dashboard from the materialized view
curl -s http://localhost:8080/api/dashboard/sales | jq

# Manually refresh the materialized view
curl -s -X POST http://localhost:8080/api/admin/refresh

# Bulk order — succeeds
curl -s -X POST http://localhost:8080/api/bulk-orders \
  -H "Content-Type: application/json" \
  -d '{"item_1": "Laptop", "item_2": "Mouse"}'

# Bulk order — rolls back (item_2 = "ERROR" triggers simulated failure)
curl -s -X POST http://localhost:8080/api/bulk-orders \
  -H "Content-Type: application/json" \
  -d '{"item_1": "Laptop", "item_2": "ERROR"}'
```

---

## Graceful shutdown

Both services handle `SIGINT` and `SIGTERM`. Shutdown happens in a deliberate order to avoid tearing down connections while work is still in flight.

### API service

```
SIGTERM
  1. srv.Shutdown(10s)              — stop accepting requests; wait for in-flight HTTP to finish
  2. <-cronScheduler.Stop().Done()  — wait for any running REFRESH to complete
  3. publisher.Close()              — release AMQP channel + connection
  4. redisClient.Close()            — release Redis pool
  5. db.Conn.Close()                — release Postgres pool
```

`cronScheduler.Stop()` returns a context that resolves when the currently-running job (if any) finishes. This ensures `db.Conn.Close()` never fires while a `REFRESH MATERIALIZED VIEW` is mid-query.

### Worker service

```
SIGTERM
  1. context cancelled              — worker.Run() exits the consume loop after current message finishes
  2. consumer.Close()               — release AMQP channel + connection
  3. db.Conn.Close()                — release Postgres pool
```

The worker uses `signal.NotifyContext` — the cancel signal flows into `worker.Run()` as a context cancellation. The consume loop checks `ctx.Done()` between messages, so an in-flight Postgres + ES write always completes before the process exits.

---

## Observability

### Prometheus metrics

Scraped at `GET /metrics`. Two histograms are instrumented:

| Metric | Labels | Description |
|--------|--------|-------------|
| `db_query_duration_seconds` | `op=read_dashboard` | Time to query `daily_sales_mv` |
| `db_query_duration_seconds` | `op=refresh_mv` | Time to run `REFRESH MATERIALIZED VIEW` |

### Structured logs

All log output uses `log/slog` (structured JSON-compatible). Every line carries a `component` field:

```
{"time":"...","level":"INFO","component":"api","order_id":"...","product":"Laptop"}
{"time":"...","level":"ERROR","component":"worker","order_id":"...","error":"..."}
{"time":"...","level":"INFO","component":"cron","schedule":"@hourly"}
```

Filter by component in any log aggregator:
```bash
# Docker
docker compose logs api   | jq 'select(.component=="api")'
docker compose logs worker | jq 'select(.component=="worker")'
```

### RabbitMQ Management UI

Available at http://localhost:15672 (guest/guest). Shows queue depth, message rates, and consumer status for `order_queue`.

### Kibana

Available at http://localhost:5601. Connect to the `orders` index to explore indexed documents and verify the worker is keeping ES in sync with Postgres.

---

## Volumes and data persistence

Named Docker volumes survive `docker compose down`. Data is only wiped with:

```bash
docker compose down -v
```

| Volume | Service | Contents |
|--------|---------|----------|
| `postgres_data` | Postgres | All orders, materialized view |
| `redis_data` | Redis | Write-back cache (AOF persistence) |
| `rabbitmq_data` | RabbitMQ | Durable queues and messages |
| `elasticsearch_data` | Elasticsearch | Orders search index |
