package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"checkout/internal/app"
	"checkout/internal/infra/producer"
)

// CheckoutConsumer runs N members of one Kafka consumer group. Kafka assigns each member a subset of partitions,
// so parallelism = min(N, partitions). Offsets are committed only after a message is handled (at-least-once);
// duplicates are harmless because CheckoutService.ProcessDraft claims the draft atomically.
type CheckoutConsumer struct {
	svc     *app.CheckoutService
	brokers []string
	topic   string
	groupID string
	members int
}

func NewCheckoutConsumer(svc *app.CheckoutService, brokers []string, topic, groupID string, members int) *CheckoutConsumer {
	return &CheckoutConsumer{svc: svc, brokers: brokers, topic: topic, groupID: groupID, members: members}
}

// Run blocks until ctx is cancelled.
func (c *CheckoutConsumer) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < c.members; i++ {
		wg.Add(1)
		go func(member int) {
			defer wg.Done()
			c.runMember(ctx, member)
		}(i)
	}
	wg.Wait()
}

func (c *CheckoutConsumer) runMember(ctx context.Context, member int) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        c.brokers,
		GroupID:        c.groupID,
		Topic:          c.topic,
		MinBytes:       1,
		MaxBytes:       1 << 20,
		MaxWait:        500 * time.Millisecond,
		CommitInterval: 0, // synchronous commit after each message
		StartOffset:    kafka.FirstOffset,
	})
	defer reader.Close()
	slog.Info("checkout consumer started", "member", member, "topic", c.topic, "group", c.groupID)

	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("fetch kafka message failed", "member", member, "err", err)
			time.Sleep(time.Second)
			continue
		}

		var ev producer.CheckoutRequestedEvent
		if err := json.Unmarshal(msg.Value, &ev); err != nil || ev.DraftID == "" {
			slog.Error("drop malformed message", "partition", msg.Partition, "offset", msg.Offset)
		} else if err := c.svc.ProcessDraft(ctx, ev.DraftID); err != nil {
			// We still commit (this is how the old message is dropped): on a transient error the draft was released
			// back to pending with a backoff, and the reclaim job publishes a new message when it is due.
			// This keeps one bad draft from blocking the whole partition.
			slog.Error("process draft failed", "draft_id", ev.DraftID, "partition", msg.Partition, "err", err)
		}

		if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			slog.Error("commit offset failed", "partition", msg.Partition, "offset", msg.Offset, "err", err)
		}
	}
}
