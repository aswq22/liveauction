package realtime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	v1 "live-auction-bid/backend/api/auction/service/v1"
)

const (
	defaultRabbitMQURL        = "amqp://guest:guest@127.0.0.1:5672/"
	defaultRabbitMQExchange   = "auction.events"
	defaultRabbitMQRoutingKey = "auction.realtime.event"
	defaultRabbitMQQueue      = "auction.realtime.websocket"
	defaultRabbitMQConsumer   = "auction-realtime"
	defaultRabbitMQPrefetch   = 100
)

type RabbitMQBusConfig struct {
	URL        string
	Exchange   string
	RoutingKey string
	Queue      string
	Consumer   string
	Origin     string
	Prefetch   int
	Durable    bool
}

type RabbitMQBus struct {
	url        string
	exchange   string
	routingKey string
	queue      string
	consumer   string
	origin     string
	prefetch   int
	durable    bool

	mu        sync.Mutex
	publishMu sync.Mutex
	conn      *amqp.Connection
	publishCh *amqp.Channel
	consumeCh *amqp.Channel
	confirms  <-chan amqp.Confirmation
}

func NewRabbitMQBus(cfg RabbitMQBusConfig) (*RabbitMQBus, error) {
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.Exchange = strings.TrimSpace(cfg.Exchange)
	cfg.RoutingKey = strings.TrimSpace(cfg.RoutingKey)
	cfg.Queue = strings.TrimSpace(cfg.Queue)
	cfg.Consumer = strings.TrimSpace(cfg.Consumer)
	cfg.Origin = strings.TrimSpace(cfg.Origin)
	if cfg.URL == "" {
		cfg.URL = defaultRabbitMQURL
	}
	if cfg.Exchange == "" {
		cfg.Exchange = defaultRabbitMQExchange
	}
	if cfg.RoutingKey == "" {
		cfg.RoutingKey = defaultRabbitMQRoutingKey
	}
	if cfg.Queue == "" {
		cfg.Queue = defaultRabbitMQQueue
	}
	if cfg.Consumer == "" {
		cfg.Consumer = cfg.Origin
	}
	if cfg.Consumer == "" {
		cfg.Consumer = defaultRabbitMQConsumer
	}
	if cfg.Origin == "" {
		cfg.Origin = cfg.Consumer
	}
	if cfg.Prefetch <= 0 {
		cfg.Prefetch = defaultRabbitMQPrefetch
	}
	return &RabbitMQBus{
		url:        cfg.URL,
		exchange:   cfg.Exchange,
		routingKey: cfg.RoutingKey,
		queue:      cfg.Queue,
		consumer:   cfg.Consumer,
		origin:     cfg.Origin,
		prefetch:   cfg.Prefetch,
		durable:    true,
	}, nil
}

func (b *RabbitMQBus) Start(ctx context.Context, sink EventPublisher) error {
	if b == nil {
		return errors.New("rabbitmq realtime bus is not initialized")
	}
	if sink == nil {
		return errors.New("rabbitmq realtime bus sink is required")
	}
	conn, publishCh, consumeCh, confirms, err := b.connect()
	if err != nil {
		return err
	}
	deliveries, err := consumeCh.Consume(
		b.queue,
		b.consumer,
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		_ = consumeCh.Close()
		_ = publishCh.Close()
		_ = conn.Close()
		return err
	}
	b.mu.Lock()
	if b.consumeCh != nil {
		_ = b.consumeCh.Close()
	}
	if b.publishCh != nil {
		_ = b.publishCh.Close()
	}
	if b.conn != nil {
		_ = b.conn.Close()
	}
	b.conn = conn
	b.publishCh = publishCh
	b.consumeCh = consumeCh
	b.confirms = confirms
	b.mu.Unlock()
	go b.receive(ctx, sink, deliveries)
	go func() {
		<-ctx.Done()
		_ = b.Close()
	}()
	return nil
}

func (b *RabbitMQBus) Publish(ctx context.Context, event v1.AuctionEvent) error {
	if b == nil {
		return errors.New("rabbitmq realtime bus is not initialized")
	}
	payload, err := encodeRedisBusEnvelope(b.origin, event)
	if err != nil {
		return err
	}
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	ch, confirms, err := b.channelForPublish()
	if err != nil {
		return err
	}
	if err := ch.PublishWithContext(ctx,
		b.exchange,
		b.routingKey,
		false,
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    event.GetId(),
			Type:         event.GetType().String(),
			Body:         []byte(payload),
		},
	); err != nil {
		return err
	}
	return waitRabbitMQConfirm(ctx, confirms)
}

func (b *RabbitMQBus) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	publishCh := b.publishCh
	consumeCh := b.consumeCh
	conn := b.conn
	b.publishCh = nil
	b.consumeCh = nil
	b.conn = nil
	b.confirms = nil
	b.mu.Unlock()
	if consumeCh != nil {
		_ = consumeCh.Close()
	}
	if publishCh != nil {
		_ = publishCh.Close()
	}
	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (b *RabbitMQBus) connect() (*amqp.Connection, *amqp.Channel, *amqp.Channel, <-chan amqp.Confirmation, error) {
	conn, err := amqp.Dial(b.url)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	publishCh, confirms, err := b.openPublisherChannel(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, nil, err
	}
	consumeCh, err := conn.Channel()
	if err != nil {
		_ = publishCh.Close()
		_ = conn.Close()
		return nil, nil, nil, nil, err
	}
	if err := consumeCh.Qos(b.prefetch, 0, false); err != nil {
		_ = consumeCh.Close()
		_ = publishCh.Close()
		_ = conn.Close()
		return nil, nil, nil, nil, err
	}
	return conn, publishCh, consumeCh, confirms, nil
}

func (b *RabbitMQBus) openPublisherChannel(conn *amqp.Connection) (*amqp.Channel, <-chan amqp.Confirmation, error) {
	if conn == nil || conn.IsClosed() {
		return nil, nil, errors.New("rabbitmq connection is not available")
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, nil, err
	}
	if err := ch.ExchangeDeclare(b.exchange, "topic", b.durable, false, false, false, nil); err != nil {
		_ = ch.Close()
		return nil, nil, err
	}
	if _, err := ch.QueueDeclare(b.queue, b.durable, false, false, false, nil); err != nil {
		_ = ch.Close()
		return nil, nil, err
	}
	if err := ch.QueueBind(b.queue, b.routingKey, b.exchange, false, nil); err != nil {
		_ = ch.Close()
		return nil, nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, nil, err
	}
	return ch, ch.NotifyPublish(make(chan amqp.Confirmation, 1)), nil
}

func (b *RabbitMQBus) channelForPublish() (*amqp.Channel, <-chan amqp.Confirmation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.publishCh != nil && !b.publishCh.IsClosed() {
		return b.publishCh, b.confirms, nil
	}
	conn := b.conn
	if conn == nil || conn.IsClosed() {
		var err error
		conn, err = amqp.Dial(b.url)
		if err != nil {
			return nil, nil, err
		}
		if b.conn != nil {
			_ = b.conn.Close()
		}
		b.conn = conn
	}
	publishCh, confirms, err := b.openPublisherChannel(conn)
	if err != nil {
		return nil, nil, err
	}
	if b.publishCh != nil {
		_ = b.publishCh.Close()
	}
	b.publishCh = publishCh
	b.confirms = confirms
	return publishCh, confirms, nil
}

func waitRabbitMQConfirm(ctx context.Context, confirms <-chan amqp.Confirmation) error {
	if confirms == nil {
		return errors.New("rabbitmq publisher confirm channel is not initialized")
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("rabbitmq publisher confirm timeout")
	case confirm, ok := <-confirms:
		if !ok {
			return errors.New("rabbitmq publisher confirm channel closed")
		}
		if !confirm.Ack {
			return errors.New("rabbitmq publisher nack")
		}
		return nil
	}
}

func (b *RabbitMQBus) receive(ctx context.Context, sink EventPublisher, deliveries <-chan amqp.Delivery) {
	for {
		select {
		case <-ctx.Done():
			return
		case delivery, ok := <-deliveries:
			if !ok {
				return
			}
			delivered, err := dispatchRedisBusPayload(ctx, b.origin, sink, string(delivery.Body))
			if err != nil {
				slog.Warn("rabbitmq realtime bus dispatch failed", "exchange", b.exchange, "queue", b.queue, "message_id", delivery.MessageId, "error", err)
				_ = delivery.Nack(false, true)
				continue
			}
			if err := delivery.Ack(false); err != nil {
				slog.Warn("rabbitmq realtime bus ack failed", "exchange", b.exchange, "queue", b.queue, "message_id", delivery.MessageId, "error", err)
				continue
			}
			if delivered {
				slog.Debug("rabbitmq realtime bus delivered event", "exchange", b.exchange, "queue", b.queue)
			}
		}
	}
}
