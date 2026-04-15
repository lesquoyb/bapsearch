package main

import (
	"encoding/json"
	"sync"
)

type SSEEvent struct {
	Name string
	Data string
}

// CardEvent is published when a result card's status, text, or score changes.
type CardEvent struct {
	URL             string  `json:"url"`
	Status          string  `json:"status,omitempty"`
	Detail          string  `json:"detail,omitempty"`
	SourceText      string  `json:"source_text,omitempty"`
	SimilarityScore float64 `json:"similarity_score,omitempty"`
}

// PipelineEvent is published when the overall pipeline status changes.
type PipelineEvent struct {
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	ReadyCount int    `json:"ready_count"`
	Target     int    `json:"target"`
}

type EventBroker struct {
	mu   sync.RWMutex
	subs map[int64]map[chan SSEEvent]struct{}
}

func NewEventBroker() *EventBroker {
	return &EventBroker{subs: make(map[int64]map[chan SSEEvent]struct{})}
}

func (b *EventBroker) Subscribe(conversationID int64) chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	b.mu.Lock()
	if b.subs[conversationID] == nil {
		b.subs[conversationID] = make(map[chan SSEEvent]struct{})
	}
	b.subs[conversationID][ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *EventBroker) Unsubscribe(conversationID int64, ch chan SSEEvent) {
	b.mu.Lock()
	delete(b.subs[conversationID], ch)
	if len(b.subs[conversationID]) == 0 {
		delete(b.subs, conversationID)
	}
	b.mu.Unlock()
}

func (b *EventBroker) Publish(conversationID int64, name string, data any) {
	j, err := json.Marshal(data)
	if err != nil {
		return
	}
	evt := SSEEvent{Name: name, Data: string(j)}
	b.mu.RLock()
	for ch := range b.subs[conversationID] {
		select {
		case ch <- evt:
		default: // drop if consumer is too slow
		}
	}
	b.mu.RUnlock()
}
