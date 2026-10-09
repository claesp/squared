// Package queue sends tasks from the API server to the worker through
// RabbitMQ.
//
// Topology:
//
//	exchange "squared.tasks" (direct)  -> queue "squared.tasks" (quorum)
//	exchange "squared.retry" (direct)  -> queue "squared.tasks.retry" (TTL per message,
//	                                       dead letters go back to "squared.tasks")
//	default exchange                   -> queue "squared.tasks.dead"
//
// The worker acknowledges a message only after it completes the task or
// moves the message to the retry or dead queue. Thus no task is lost when a
// worker stops.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"squared/internal/auth"
	"squared/internal/domain"
)

// Names of the RabbitMQ objects.
const (
	TasksExchange = "squared.tasks"
	RetryExchange = "squared.retry"
	TasksQueue    = "squared.tasks"
	RetryQueue    = "squared.tasks.retry"
	DeadQueue     = "squared.tasks.dead"
	routingKey    = "task"
	attemptHeader = "x-squared-attempt"
)

// Declare makes the exchanges and queues. It is safe to call it more than
// one time.
func Declare(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(TasksExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", TasksExchange, err)
	}
	if err := ch.ExchangeDeclare(RetryExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", RetryExchange, err)
	}
	if _, err := ch.QueueDeclare(TasksQueue, true, false, false, false, amqp.Table{
		"x-queue-type": "quorum",
	}); err != nil {
		return fmt.Errorf("declare queue %s: %w", TasksQueue, err)
	}
	if _, err := ch.QueueDeclare(RetryQueue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    TasksExchange,
		"x-dead-letter-routing-key": routingKey,
	}); err != nil {
		return fmt.Errorf("declare queue %s: %w", RetryQueue, err)
	}
	if _, err := ch.QueueDeclare(DeadQueue, true, false, false, false, amqp.Table{
		"x-queue-type": "quorum",
	}); err != nil {
		return fmt.Errorf("declare queue %s: %w", DeadQueue, err)
	}
	if err := ch.QueueBind(TasksQueue, routingKey, TasksExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %s: %w", TasksQueue, err)
	}
	if err := ch.QueueBind(RetryQueue, routingKey, RetryExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %s: %w", RetryQueue, err)
	}
	return nil
}

// Publisher sends messages with publisher confirms. It connects again
// after a connection failure. A Publisher is safe for concurrent use.
type Publisher struct {
	url string
	log *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

// NewPublisher returns a Publisher. It connects on the first use.
func NewPublisher(url string, log *slog.Logger) *Publisher {
	return &Publisher{url: url, log: log}
}

func (p *Publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	p.closeLocked()
	conn, err := amqp.DialConfig(p.url, amqp.Config{Heartbeat: 10 * time.Second, Properties: amqp.Table{"connection_name": "squared-publisher"}})
	if err != nil {
		return nil, fmt.Errorf("connect to message broker: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := Declare(ch); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	p.conn, p.ch = conn, ch
	return ch, nil
}

func (p *Publisher) closeLocked() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn, p.ch = nil, nil
}

// Close closes the connection.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
}

// Ping connects to the broker if necessary.
func (p *Publisher) Ping() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.channel()
	return err
}

// Publish sends one message and waits until the broker confirms it.
func (p *Publisher) Publish(ctx context.Context, exchange, key string, msg amqp.Publishing) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, err := p.channel()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchange, key, true, false, msg)
	if err != nil {
		p.closeLocked()
		return fmt.Errorf("publish: %w", err)
	}
	ok, err := conf.WaitContext(ctx)
	if err != nil {
		p.closeLocked()
		return fmt.Errorf("publish: wait for confirm: %w", err)
	}
	if !ok {
		return errors.New("publish: the broker did not accept the message")
	}
	return nil
}

// PublishTask sends a task payload (JSON) to the task queue.
func (p *Publisher) PublishTask(ctx context.Context, payload []byte) error {
	return p.Publish(ctx, TasksExchange, routingKey, message(payload, 1))
}

func message(payload []byte, attempt int) amqp.Publishing {
	return amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    auth.NewToken("msg_"),
		Timestamp:    time.Now(),
		Headers:      amqp.Table{attemptHeader: int32(attempt)},
		Body:         payload,
	}
}

// Permanent marks an error that a retry cannot repair, for example
// invalid input. The consumer does not retry such tasks.
func Permanent(err error) error { return permanentError{err} }

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// IsPermanent tells if err is marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Handler processes one task. attempt starts at 1.
type Handler interface {
	Handle(ctx context.Context, task domain.Task, attempt int) error
	// GiveUp is called when the consumer stops the retries of a task.
	GiveUp(ctx context.Context, task domain.Task, err error)
}

// ConsumerConfig configures a Consumer.
type ConsumerConfig struct {
	URL         string
	Concurrency int           // Number of tasks in progress at the same time.
	MaxAttempts int           // Attempts before a task goes to the dead queue.
	BaseDelay   time.Duration // Delay before the first retry. Each retry doubles it.
	MaxDelay    time.Duration
	TaskTimeout time.Duration
}

// Consumer receives tasks and gives them to a Handler.
type Consumer struct {
	cfg     ConsumerConfig
	handler Handler
	log     *slog.Logger
	pub     *Publisher
}

// NewConsumer returns a Consumer.
func NewConsumer(cfg ConsumerConfig, h Handler, log *slog.Logger) *Consumer {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 12
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 2 * time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 2 * time.Minute
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 2 * time.Minute
	}
	return &Consumer{cfg: cfg, handler: h, log: log, pub: NewPublisher(cfg.URL, log)}
}

// Run receives tasks until ctx is done. It connects again after failures.
func (c *Consumer) Run(ctx context.Context) error {
	defer c.pub.Close()
	delay := time.Second
	for {
		err := c.consume(ctx)
		if ctx.Err() != nil {
			return nil
		}
		c.log.Error("consumer stopped; connecting again", "error", err, "delay", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (c *Consumer) consume(ctx context.Context) error {
	conn, err := amqp.DialConfig(c.cfg.URL, amqp.Config{Heartbeat: 10 * time.Second, Properties: amqp.Table{"connection_name": "squared-worker"}})
	if err != nil {
		return fmt.Errorf("connect to message broker: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	if err := Declare(ch); err != nil {
		return err
	}
	if err := ch.Qos(c.cfg.Concurrency, 0, false); err != nil {
		return fmt.Errorf("set prefetch: %w", err)
	}
	deliveries, err := ch.ConsumeWithContext(ctx, TasksQueue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	c.log.Info("consuming tasks", "queue", TasksQueue, "concurrency", c.cfg.Concurrency)

	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, c.cfg.Concurrency)
	for {
		select {
		case <-ctx.Done():
			return nil
		case amqpErr := <-closed:
			return fmt.Errorf("connection closed: %v", amqpErr)
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("delivery channel closed")
			}
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				c.process(ctx, d)
			}()
		}
	}
}

func (c *Consumer) process(ctx context.Context, d amqp.Delivery) {
	attempt := 1
	if v, ok := d.Headers[attemptHeader].(int32); ok && v > 0 {
		attempt = int(v)
	}
	var task domain.Task
	if err := json.Unmarshal(d.Body, &task); err != nil || task.Kind == "" {
		c.log.Error("bad task message; moving it to the dead queue", "error", err, "message_id", d.MessageId)
		c.toDead(ctx, d, attempt)
		return
	}
	log := c.log.With("kind", task.Kind, "tenant_id", task.TenantID, "deployment_id", task.DeploymentID, "attempt", attempt)

	taskCtx, cancel := context.WithTimeout(ctx, c.cfg.TaskTimeout)
	err := c.handle(taskCtx, task, attempt)
	cancel()
	if err == nil {
		log.Info("task done")
		c.ack(d, log)
		return
	}
	if ctx.Err() != nil {
		// The worker stops. Give the message back to the broker.
		_ = d.Nack(false, true)
		return
	}
	if IsPermanent(err) || attempt >= c.cfg.MaxAttempts {
		log.Error("task failed; no more retries", "error", err)
		c.handler.GiveUp(ctx, task, err)
		c.toDead(ctx, d, attempt)
		return
	}
	delay := c.delay(attempt)
	log.Warn("task failed; retry later", "error", err, "delay", delay)
	msg := message(d.Body, attempt+1)
	msg.MessageId = d.MessageId
	msg.Expiration = fmt.Sprint(delay.Milliseconds())
	if err := c.pub.Publish(ctx, RetryExchange, routingKey, msg); err != nil {
		log.Error("cannot schedule retry; message goes back to the queue", "error", err)
		_ = d.Nack(false, true)
		return
	}
	c.ack(d, log)
}

// handle calls the handler and changes a panic into an error, so that one
// bad task does not stop the worker.
func (c *Consumer) handle(ctx context.Context, task domain.Task, attempt int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return c.handler.Handle(ctx, task, attempt)
}

func (c *Consumer) delay(attempt int) time.Duration {
	d := c.cfg.BaseDelay
	for i := 1; i < attempt && d < c.cfg.MaxDelay; i++ {
		d *= 2
	}
	return min(d, c.cfg.MaxDelay)
}

func (c *Consumer) toDead(ctx context.Context, d amqp.Delivery, attempt int) {
	msg := message(d.Body, attempt)
	msg.MessageId = d.MessageId
	if err := c.pub.Publish(ctx, "", DeadQueue, msg); err != nil {
		c.log.Error("cannot move message to the dead queue; message goes back to the queue", "error", err)
		_ = d.Nack(false, true)
		return
	}
	c.ack(d, c.log)
}

func (c *Consumer) ack(d amqp.Delivery, log *slog.Logger) {
	if err := d.Ack(false); err != nil {
		// The broker sends the message again. Tasks are idempotent.
		log.Error("acknowledge message", "error", err)
	}
}
