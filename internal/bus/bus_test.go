package bus_test

import (
	"context"
	"testing"
	"time"

	"zing/internal/bus"
)

// waitTimeout bounds every waitBounded call below: long enough that a slow
// CI runner never trips it, short enough that a real leak fails fast.
const waitTimeout = time.Second

// waitBounded reads from ch, failing the test if nothing arrives within
// waitTimeout. This is a bounded wait over channel synchronization, not a
// sleep race.
func waitBounded(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatal(msg)
	}
}

func TestBroker_PublishCoalescesBurstToOneWake(t *testing.T) {
	t.Parallel()

	b := bus.New()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish()
	b.Publish()
	b.Publish()

	waitBounded(t, ch, "expected a wake after a burst of Publish calls")

	select {
	case <-ch:
		t.Fatal("expected the burst to coalesce into one wake, got a second")
	default:
	}
}

func TestBroker_SubscribeDeliversOnPublish(t *testing.T) {
	t.Parallel()

	b := bus.New()
	ch, cancel := b.Subscribe()
	defer cancel()

	select {
	case <-ch:
		t.Fatal("received a wake before any Publish")
	default:
	}

	b.Publish()
	waitBounded(t, ch, "expected a wake after Publish")
}

func TestBroker_CancelStopsFurtherDelivery(t *testing.T) {
	t.Parallel()

	b := bus.New()
	ch, cancel := b.Subscribe()
	cancel()

	b.Publish()

	select {
	case <-ch:
		t.Fatal("received a wake on a cancelled subscription")
	default:
	}
}

// TestBroker_PublishNeverBlocksOnAFullLiveSubscriberBuffer proves the
// non-blocking send claim on a subscriber that is actually live: a fresh
// subscription's one-slot buffer is filled by a first Publish and never
// drained, so the second Publish below must hit the select's default case
// on a full channel, not skip an absent one. It also confirms the buffer
// coalesces rather than silently dropping: the subscriber still sees
// exactly one pending wake afterward.
func TestBroker_PublishNeverBlocksOnAFullLiveSubscriberBuffer(t *testing.T) {
	t.Parallel()

	b := bus.New()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish() // fills the subscriber's 1-slot buffer; left undrained on purpose

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Publish()
	}()

	waitBounded(t, done, "Publish blocked on a live subscriber whose buffer was already full")

	waitBounded(t, ch, "expected the live subscriber to have a pending wake")
	select {
	case <-ch:
		t.Fatal("expected exactly one coalesced wake, got a second")
	default:
	}
}

// TestBroker_CancelledSubscriberReaderLeaksNoGoroutine proves the pattern the
// plan requires of every reader: select on the subscription channel and on
// the request context, so a client that goes away lets the reader goroutine
// exit instead of leaking. The wait for that exit is bounded by a channel
// close, never a sleep race.
func TestBroker_CancelledSubscriberReaderLeaksNoGoroutine(t *testing.T) {
	t.Parallel()

	b := bus.New()
	ch, cancel := b.Subscribe()

	ctx, stop := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ch:
		case <-ctx.Done():
		}
	}()

	// Simulate the client going away: the request context is cancelled and
	// the reader's own subscription is torn down.
	stop()
	cancel()

	waitBounded(t, exited, "reader goroutine did not exit after its context was cancelled")
}
