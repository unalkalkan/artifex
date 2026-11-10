// Package events passes project changes to open browser views (design
// decision 21). Delivery is best effort: a slow subscriber misses events
// rather than holding up a publish.
package events

import "sync"

// Event is one change in a project.
type Event struct {
	Type    string   `json:"type"` // "version" or "thread"
	Project string   `json:"project"`
	Version string   `json:"version,omitempty"` // the new head, for "version"
	Paths   []string `json:"paths,omitempty"`   // changed files, for "version"
	Thread  int64    `json:"thread,omitempty"`  // for "thread"
	Path    string   `json:"path,omitempty"`    // the thread's artifact
}

type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[string]map[chan Event]struct{}{}} }

// Subscribe returns a channel of the project's events and a function that
// ends the subscription.
func (h *Hub) Subscribe(project string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	if h.subs[project] == nil {
		h.subs[project] = map[chan Event]struct{}{}
	}
	h.subs[project][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[project][ch]; ok {
			delete(h.subs[project], ch)
			close(ch)
		}
		if len(h.subs[project]) == 0 {
			delete(h.subs, project)
		}
		h.mu.Unlock()
	}
}

// Publish sends e to the project's subscribers without blocking.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[e.Project] {
		select {
		case ch <- e:
		default:
		}
	}
}
