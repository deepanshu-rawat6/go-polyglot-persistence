// Package cache provides a Redis-backed write-back cache for Order objects.
//
// Write-back pattern:
//   - On write:  data is stored in Redis immediately and published to the queue.
//     The caller gets an instant 202 Accepted response.
//     The background worker is responsible for durably persisting to Postgres.
//   - On read:   Redis is checked first (cache HIT). On a miss, the caller falls back
//     to Postgres and back-fills the cache for subsequent requests.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go-polyglot-persistence/internal/models"

	"github.com/redis/go-redis/v9"
)

const (
	orderKeyPrefix = "order:"
	orderTTL       = 24 * time.Hour
)

// ErrNotFound is returned when a key does not exist in the cache.
var ErrNotFound = errors.New("cache: key not found")

// Client wraps the Redis client and exposes domain-level operations.
type Client struct {
	rdb *redis.Client
}

// New creates a Redis client and verifies the connection with a PING.
func New(addr string) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}

	return &Client{rdb: rdb}, nil
}

// Close shuts down the underlying connection pool.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// SetOrder serialises an Order and stores it in Redis with a fixed TTL.
func (c *Client) SetOrder(ctx context.Context, order *models.Order) error {
	data, err := json.Marshal(order)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, orderKeyPrefix+order.ID, data, orderTTL).Err()
}

// GetOrder fetches an Order by ID from Redis.
// Returns ErrNotFound when the key does not exist or has expired.
func (c *Client) GetOrder(ctx context.Context, id string) (*models.Order, error) {
	data, err := c.rdb.Get(ctx, orderKeyPrefix+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var order models.Order
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, err
	}
	return &order, nil
}
