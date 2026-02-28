// Package queue wraps RabbitMQ for reliable, decoupled message passing.
//
// The API service publishes Order events to the "order_queue".
// The worker service consumes from the same queue and persists data to Postgres + Elasticsearch.
//
// Durability guarantees:
//   - Queue is declared as durable — survives broker restarts.
//   - Messages are marked as Persistent — written to disk before ack.
//   - Consumer uses manual ack — a message is only removed from the queue
//     after the worker has successfully written to both Postgres and ES.
package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"go-polyglot-persistence/internal/models"

	amqp "github.com/rabbitmq/amqp091-go"
)

const orderQueueName = "order_queue"

// Publisher owns the AMQP connection for the API service side (publish only).
type Publisher struct {
	conn    *amqp.Connection
	channel *amqp.Channel
	queue   amqp.Queue
}

// NewPublisher dials RabbitMQ and declares the shared queue.
func NewPublisher(url string) (*Publisher, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("queue: dial: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("queue: open channel: %w", err)
	}

	q, err := declareQueue(ch)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	return &Publisher{conn: conn, channel: ch, queue: q}, nil
}

// PublishOrder serialises the order and sends it to the queue.
// The message is marked Persistent so it survives a broker restart.
func (p *Publisher) PublishOrder(ctx context.Context, order *models.Order) error {
	body, err := json.Marshal(order)
	if err != nil {
		return err
	}

	return p.channel.PublishWithContext(ctx,
		"",           // default exchange — routes directly to named queue
		p.queue.Name, // routing key == queue name for default exchange
		false,        // mandatory
		false,        // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent, // survive broker restart
			Body:         body,
		},
	)
}

// Close releases the AMQP channel and connection.
func (p *Publisher) Close() {
	p.channel.Close()
	p.conn.Close()
}

// Consumer owns the AMQP connection for the worker side (consume only).
type Consumer struct {
	conn    *amqp.Connection
	channel *amqp.Channel
	queue   amqp.Queue
}

// NewConsumer dials RabbitMQ and sets QoS to process one message at a time.
func NewConsumer(url string) (*Consumer, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("queue: dial: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("queue: open channel: %w", err)
	}

	// Process one message at a time — prevents one slow consumer from hoarding.
	if err := ch.Qos(1, 0, false); err != nil {
		ch.Close()
		conn.Close()
		return nil, fmt.Errorf("queue: set qos: %w", err)
	}

	q, err := declareQueue(ch)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}

	return &Consumer{conn: conn, channel: ch, queue: q}, nil
}

// Delivery wraps amqp.Delivery to expose the decoded Order and ack/nack helpers.
type Delivery struct {
	Order *models.Order
	raw   amqp.Delivery
}

// Ack removes the message from RabbitMQ after successful processing.
func (d *Delivery) Ack() error { return d.raw.Ack(false) }

// Nack requeues the message so another worker can retry.
func (d *Delivery) Nack() error { return d.raw.Nack(false, true) }

// Discard permanently rejects a message (e.g. unparseable payload).
func (d *Delivery) Discard() error { return d.raw.Nack(false, false) }

// Consume returns a channel of Delivery values. Each value must be Ack'd or Nack'd.
func (c *Consumer) Consume() (<-chan Delivery, error) {
	rawMsgs, err := c.channel.Consume(
		c.queue.Name,
		"",    // consumer tag — auto-generated
		false, // auto-ack disabled — we ack manually after successful processing
		false, // exclusive
		false, // no-local
		false, // no-wait
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: consume: %w", err)
	}

	out := make(chan Delivery)
	go func() {
		defer close(out)
		for d := range rawMsgs {
			var order models.Order
			if err := json.Unmarshal(d.Body, &order); err != nil {
				// Discard unparseable messages — they will never be valid.
				d.Nack(false, false)
				continue
			}
			out <- Delivery{Order: &order, raw: d}
		}
	}()

	return out, nil
}

// Close releases the AMQP channel and connection.
func (c *Consumer) Close() {
	c.channel.Close()
	c.conn.Close()
}

// declareQueue is shared between Publisher and Consumer to ensure both sides
// always declare the same durable queue (idempotent — safe to call multiple times).
func declareQueue(ch *amqp.Channel) (amqp.Queue, error) {
	q, err := ch.QueueDeclare(
		orderQueueName,
		true,  // durable — survives broker restart
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	)
	if err != nil {
		return amqp.Queue{}, fmt.Errorf("queue: declare: %w", err)
	}
	return q, nil
}
