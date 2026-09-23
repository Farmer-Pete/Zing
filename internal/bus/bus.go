// Package bus is the in-process change notifier the dispatcher and the
// console share. A published change carries no payload; a reader wakes and
// re-reads the store.
package bus

import "sync"

// Broker fans a bare "something changed" signal out to every subscriber.
// One writer, many readers.
type Broker struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

// New returns a Broker with no subscribers.
func New() *Broker {
	return &Broker{subscribers: make(map[chan struct{}]struct{})}
}

// Publish wakes every current subscriber with a non-blocking send, so a
// burst of changes coalesces into one wake for a subscriber that has not
// read the last one yet.
func (b *Broker) Publish() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe registers a new subscriber and returns its channel, buffered to
// one, and a cancel func that removes it. A reader must select on both ch
// and its own request context, so a client that goes away lets the reader
// exit instead of leaking; cancel then drops the channel from future
// Publish calls.
func (b *Broker) Subscribe() (ch <-chan struct{}, cancel func()) {
	c := make(chan struct{}, 1)

	b.mu.Lock()
	b.subscribers[c] = struct{}{}
	b.mu.Unlock()

	cancel = func() {
		b.mu.Lock()
		delete(b.subscribers, c)
		b.mu.Unlock()
	}
	return c, cancel
}
