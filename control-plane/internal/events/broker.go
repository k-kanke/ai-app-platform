// Package events fans out app progress events to SSE subscribers.
package events

import (
	"sync"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/store"
)

type Broker struct {
	mu   sync.Mutex
	subs map[string]map[chan store.Event]struct{}
}

func NewBroker() *Broker { return &Broker{subs: map[string]map[chan store.Event]struct{}{}} }

// Subscribe returns a channel of events for appID and an unsubscribe func.
func (b *Broker) Subscribe(appID string) (<-chan store.Event, func()) {
	ch := make(chan store.Event, 64)
	b.mu.Lock()
	if b.subs[appID] == nil {
		b.subs[appID] = map[chan store.Event]struct{}{}
	}
	b.subs[appID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[appID], ch)
		b.mu.Unlock()
	}
}

// Publish never blocks: a slow subscriber drops events and recovers via Last-Event-ID replay.
func (b *Broker) Publish(e store.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[e.AppID] {
		select {
		case ch <- e:
		default:
		}
	}
}
