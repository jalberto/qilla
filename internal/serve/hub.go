package serve

import (
	"encoding/json"
	"sync"
	"time"
)

// Event is one SSE message.
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // job, run, warn, info
	Msg  string    `json:"msg"`
	Data any       `json:"data,omitempty"`
}

// hub fans events out to SSE subscribers and keeps a short replay buffer.
type hub struct {
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	recent  []Event
	closing chan struct{} // closed when serve stops: every SSE handler returns
}

func newHub() *hub { return &hub{subs: map[chan Event]struct{}{}, closing: make(chan struct{})} }

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.closing:
	default:
		close(h.closing)
	}
}

func (h *hub) publish(kind, msg string, data any) {
	e := Event{Time: time.Now(), Kind: kind, Msg: msg, Data: data}
	h.mu.Lock()
	h.recent = append(h.recent, e)
	if len(h.recent) > 100 {
		h.recent = h.recent[len(h.recent)-100:]
	}
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // slow client: drop
		}
	}
	h.mu.Unlock()
}

func (h *hub) subscribe() (chan Event, []Event, func()) {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	replay := append([]Event(nil), h.recent...)
	h.mu.Unlock()
	return ch, replay, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (e Event) line() []byte {
	b, _ := json.Marshal(e)
	return b
}
