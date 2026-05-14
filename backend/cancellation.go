package main

import (
	"context"
	"sync"
)

// Cancellations tracks the active context cancel functions for each
// conversation so the UI can interrupt an in-flight pipeline (search,
// embedding, rewrite) or answer stream without waiting for natural completion.
//
// Each registered cancel may be triggered multiple times safely; CancelAll
// removes the slot after firing every entry.
type Cancellations struct {
	mu      sync.Mutex
	cancels map[int64]map[uint64]context.CancelFunc
	next    uint64
}

func NewCancellations() *Cancellations {
	return &Cancellations{cancels: make(map[int64]map[uint64]context.CancelFunc)}
}

// Register associates cancel with conversationID and returns a deregister
// function the caller should defer to clean up once the operation finishes
// normally.
func (c *Cancellations) Register(conversationID int64, cancel context.CancelFunc) func() {
	c.mu.Lock()
	c.next++
	id := c.next
	if c.cancels[conversationID] == nil {
		c.cancels[conversationID] = make(map[uint64]context.CancelFunc)
	}
	c.cancels[conversationID][id] = cancel
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		if m, ok := c.cancels[conversationID]; ok {
			delete(m, id)
			if len(m) == 0 {
				delete(c.cancels, conversationID)
			}
		}
		c.mu.Unlock()
	}
}

// CancelAll fires every cancel registered for conversationID and clears the
// slot. Returns the number of cancels that were triggered.
func (c *Cancellations) CancelAll(conversationID int64) int {
	c.mu.Lock()
	m := c.cancels[conversationID]
	delete(c.cancels, conversationID)
	c.mu.Unlock()
	for _, cancel := range m {
		cancel()
	}
	return len(m)
}
