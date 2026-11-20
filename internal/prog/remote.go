package prog

import (
	"context"
	"sync"
	"time"
)

type remoteAttempt struct {
	context    context.Context
	generation uint64
}

type remoteGate struct {
	mu         sync.Mutex
	parent     context.Context
	context    context.Context
	cancel     context.CancelFunc
	generation uint64
	next       time.Time
	delay      time.Duration
	maximum    time.Duration
	probing    bool
	strict     bool
}

func newRemoteGate(parent context.Context, timeout time.Duration, strict bool) *remoteGate {
	ctx, cancel := context.WithCancel(parent)
	return &remoteGate{parent: parent, context: ctx, cancel: cancel, maximum: max(30*time.Second, timeout), strict: strict}
}

func (gate *remoteGate) begin() (remoteAttempt, bool) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.strict && !gate.next.IsZero() {
		if time.Now().Before(gate.next) || gate.probing {
			return remoteAttempt{}, false
		}
		gate.probing = true
	}
	return remoteAttempt{context: gate.context, generation: gate.generation}, true
}

func (gate *remoteGate) finish(attempt remoteAttempt, unavailable bool) {
	if gate.strict {
		return
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if attempt.generation != gate.generation || attempt.context.Err() != nil {
		return
	}
	if unavailable {
		gate.cancel()
		gate.context, gate.cancel = context.WithCancel(gate.parent)
		gate.generation++
		gate.delay = min(gate.maximum, max(time.Second, gate.delay*2))
		gate.next = time.Now().Add(gate.delay)
		gate.probing = false
	} else if gate.probing {
		gate.next = time.Time{}
		gate.delay = 0
		gate.probing = false
	}
}

func (gate *remoteGate) close() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.cancel()
}
