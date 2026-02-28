# go-polyglot-persistence

A Go project exploring **write-back caching**, **async persistence**, **full-text search**, and **materialized views** — all wired together with Redis, RabbitMQ, PostgreSQL, and Elasticsearch, running locally in Docker.

## Core idea

The API never writes to Postgres directly. It writes to Redis immediately (so reads are instant), publishes to a queue, and a background worker handles durable persistence asynchronously.

```
  HTTP Client
      │
      ▼
┌─────────────────────────┐
│       API Service       │
│  • Redis  (cache)       │
│  • RabbitMQ (publish)   │
└────────────┬────────────┘
             │ order_queue
             ▼
┌─────────────────────────┐
│      Worker Service     │
│  • Postgres  (persist)  │
│  • Elasticsearch (index)│
└─────────────────────────┘
```

## Project structure

```
go-polyglot-persistence/
├── cmd/
│   ├── api/main.go          # Wires packages, starts HTTP server
│   └── worker/main.go       # Wires packages, starts consume loop
│
├── internal/
│   ├── api/
│   │   ├── handlers.go      # One method per route; deps injected via interfaces
│   │   └── routes.go        # Route table — full API surface in one place
│   ├── cache/               # Redis write-back cache
│   ├── config/              # Env var loading with docker-compose defaults
│   ├── database/            # Postgres — all SQL lives here, context timeouts on every op
│   ├── metrics/             # Prometheus histograms
│   ├── models/              # Shared types (Order, DailySale)
│   ├── queue/               # RabbitMQ Publisher + Consumer, manual ack
│   ├── search/              # Elasticsearch index + search
│   └── worker/
│       ├── worker.go        # Worker struct — consume loop, per-message timeout
│       └── cron.go          # Hourly materialized view refresh
│
├── docs/
│   ├── architecture.md      # Data flows, design decisions, package contracts
│   └── ops.md               # Config, observability, curl examples, shutdown
│
├── Dockerfile.api            # Multi-stage build → scratch image (~5 MB)
├── Dockerfile.worker         # Same pattern
├── docker-compose.yml        # 7 services with healthchecks + named volumes
└── init.sql                  # Postgres schema bootstrap (auto-run on first start)
```

## Quickstart

```bash
docker compose up --build
```

| Service       | URL                                   |
|---------------|---------------------------------------|
| API           | http://localhost:8080                 |
| RabbitMQ UI   | http://localhost:15672  (guest/guest) |
| Kibana        | http://localhost:5601                 |
| Elasticsearch | http://localhost:9200                 |
| Postgres      | localhost:5432  (postgres/secret)     |
| Redis         | localhost:6379                        |

```bash
# Tear down and wipe all persisted data
docker compose down -v
```

## Further reading

- **[docs/architecture.md](docs/architecture.md)** — data flows, write-back cache pattern, idempotency, materialized view design
- **[docs/ops.md](docs/ops.md)** — all env vars, graceful shutdown sequence, observability, example requests
