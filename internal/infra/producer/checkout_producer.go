package producer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// CheckoutRequestedEvent is published after a draft is persisted. Only the id travels on the wire;
// the draft row in Postgres is the source of truth.
type CheckoutRequestedEvent struct {
	DraftID string `json:"draft_id"`
}

type CheckoutProducer struct {
	writer *kafka.Writer
}

func NewCheckoutProducer(brokers []string, topic string) *CheckoutProducer {
	return &CheckoutProducer{writer: &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Topic:        topic,
		Balancer:     &kafka.Hash{}, // key = draft_id -> spread evenly across partitions
		RequiredAcks: kafka.RequireAll,
		BatchTimeout: 10 * time.Millisecond,
		WriteTimeout: 5 * time.Second,
	}}
}

func (p *CheckoutProducer) PublishCheckoutRequested(ctx context.Context, draftIDs ...string) error {
	msgs := make([]kafka.Message, 0, len(draftIDs))
	for _, id := range draftIDs {
		b, err := json.Marshal(CheckoutRequestedEvent{DraftID: id})
		if err != nil {
			return err
		}
		msgs = append(msgs, kafka.Message{Key: []byte(id), Value: b})
	}
	if err := p.writer.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("publish checkout requested: %w", err)
	}
	return nil
}

func (p *CheckoutProducer) Close() error { return p.writer.Close() }

// EnsureTopic creates the topic if it does not exist yet.
func EnsureTopic(ctx context.Context, broker, topic string, partitions int) error {
	conn, err := kafka.DialContext(ctx, "tcp", broker)
	if err != nil {
		return fmt.Errorf("dial kafka: %w", err)
	}
	defer conn.Close()
	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("get kafka controller: %w", err)
	}
	cconn, err := kafka.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return fmt.Errorf("dial kafka controller: %w", err)
	}
	defer cconn.Close()
	err = cconn.CreateTopics(kafka.TopicConfig{Topic: topic, NumPartitions: partitions, ReplicationFactor: 1})
	if err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		return fmt.Errorf("create topic: %w", err)
	}
	return nil
}
