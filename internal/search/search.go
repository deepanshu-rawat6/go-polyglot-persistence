// Package search provides an Elasticsearch client for indexing and querying Orders.
//
// Why Elasticsearch over Postgres full-text search?
//   - Inverted index: sub-millisecond full-text search across millions of documents.
//   - Relevance scoring: results ranked by match quality, not insertion order.
//   - Scalability: horizontally sharded, Postgres full-text search does not shard.
//   - Aggregations: faceted search (e.g. "all orders for 'laptop' grouped by day")
//     without expensive GROUP BY scans on the primary database.
//
// Index lifecycle:
//   - The worker calls IndexOrder after every successful Postgres insert.
//   - The API calls SearchOrders to serve the GET /api/search endpoint.
//   - Postgres remains the source of truth; ES is a read-optimised projection.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"go-polyglot-persistence/internal/models"

	"github.com/elastic/go-elasticsearch/v8"
)

const ordersIndex = "orders"

// Client wraps the Elasticsearch client with domain-level operations.
type Client struct {
	es *elasticsearch.Client
}

// New creates an Elasticsearch client pointed at the given URL.
func New(url string) (*Client, error) {
	cfg := elasticsearch.Config{
		Addresses: []string{url},
	}
	es, err := elasticsearch.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("search: create client: %w", err)
	}
	return &Client{es: es}, nil
}

// IndexOrder upserts an Order document into the "orders" index.
// Using the order ID as the document ID makes this idempotent —
// re-indexing the same order on a worker retry will not create duplicates.
func (c *Client) IndexOrder(ctx context.Context, order *models.Order) error {
	body, err := json.Marshal(order)
	if err != nil {
		return err
	}

	res, err := c.es.Index(
		ordersIndex,
		bytes.NewReader(body),
		c.es.Index.WithDocumentID(order.ID),
		c.es.Index.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("search: index request: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("search: index error [%s]: %s", res.Status(), body)
	}
	return nil
}

// SearchOrders executes a full-text match query against the product_name field.
// It returns the raw Elasticsearch response body for the API to proxy directly.
func (c *Client) SearchOrders(ctx context.Context, term string) (json.RawMessage, error) {
	query := map[string]any{
		"query": map[string]any{
			"match": map[string]any{
				"product_name": term,
			},
		},
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(query); err != nil {
		return nil, err
	}

	res, err := c.es.Search(
		c.es.Search.WithContext(ctx),
		c.es.Search.WithIndex(ordersIndex),
		c.es.Search.WithBody(&buf),
		c.es.Search.WithTrackTotalHits(true),
	)
	if err != nil {
		return nil, fmt.Errorf("search: query request: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("search: query error [%s]: %s", res.Status(), body)
	}

	return io.ReadAll(res.Body)
}
