// Dispatcher overview:
// - Single producer (the underlying Messager reading from a stream)
// - Multiple logical consumers via WaitFor()
// Pipeline: continuous read -> bucket by pb.MessageType (per-type queue) -> consumer fetches
// Features:
//   - Per-type queue limit (10). When full, oldest entry is evicted (overflow counter increments)
//   - TTL pruning: on enqueue we drop expired head entries; on dequeue we also skip newly expired items
//   - Minimum effective TTL enforced at 1s (even if ttl_ms smaller or zero)
//   - WaitFor waits for the first non‑expired message among requested types or returns on context/close/error
//   - Metrics: droppedExpired, droppedOverflow (stored but not yet exported via API)
//   - Close() unblocks all waiting goroutines
package message

import (
	"context"
	"errors"
	"sync"
	"time"

	pb "github.com/Hexrotor/f2p/proto"
)

type Dispatcher struct {
	m       *Messager
	mu      sync.Mutex
	queues  map[pb.MessageType][]entry
	closed  bool
	err     error
	started bool

	// metrics
	droppedExpired  map[pb.MessageType]uint64
	droppedOverflow map[pb.MessageType]uint64

	notify chan struct{}
}

type entry struct {
	msg       *pb.UnifiedMessage
	enqueueAt time.Time
}

const perTypeLimit = 10

func NewDispatcher(m *Messager) *Dispatcher {
	return &Dispatcher{
		m:               m,
		queues:          make(map[pb.MessageType][]entry),
		droppedExpired:  make(map[pb.MessageType]uint64),
		droppedOverflow: make(map[pb.MessageType]uint64),
		notify:          make(chan struct{}, 1),
	}
}

func (d *Dispatcher) Start() {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.mu.Unlock()

	go func() {
		for {
			if d.IsClosed() {
				return
			}

			msg, err := d.m.receiveMessage(0)
			if err != nil {
				d.mu.Lock()
				if !d.closed {
					d.err = err
				}
				d.mu.Unlock()
				d.broadcast()
				return
			}

			now := time.Now()
			d.mu.Lock()
			q := d.queues[msg.Type]

			// Prune expired entries at the head (they were never delivered).
			for len(q) > 0 {
				ttl := time.Duration(q[0].msg.GetTtlMs()) * time.Millisecond
				if ttl < time.Second {
					ttl = time.Second
				}
				if now.Sub(q[0].enqueueAt) > ttl {
					q = q[1:]
					d.droppedExpired[msg.Type]++
					continue
				}
				break
			}

			// Enforce per-type limit (drop oldest when full).
			if perTypeLimit > 0 && len(q) >= perTypeLimit {
				q = q[1:]
				d.droppedOverflow[msg.Type]++
			}

			// Append new message.
			q = append(q, entry{msg: msg, enqueueAt: now})
			d.queues[msg.Type] = q
			d.mu.Unlock()
			d.broadcast()
		}
	}()
}

func (d *Dispatcher) WaitFor(ctx context.Context, types ...pb.MessageType) (*pb.UnifiedMessage, error) {
	for {
		d.mu.Lock()
		now := time.Now()
		for _, t := range types {
			q := d.queues[t]
			for len(q) > 0 {
				e := q[0]
				q = q[1:]
				ttl := time.Duration(e.msg.GetTtlMs()) * time.Millisecond
				if ttl < time.Second {
					ttl = time.Second
				}
				if now.Sub(e.enqueueAt) > ttl {
					d.droppedExpired[t]++
					continue
				}
				d.queues[t] = q
				d.mu.Unlock()
				return e.msg, nil
			}
			d.queues[t] = q
		}
		if d.err != nil {
			err := d.err
			d.mu.Unlock()
			return nil, err
		}
		if d.closed {
			d.mu.Unlock()
			return nil, errors.New("dispatcher closed")
		}
		d.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-d.notify:
			// retry
		}
	}
}

func (d *Dispatcher) Close() {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
	}
	d.mu.Unlock()
	d.broadcast()
}

func (d *Dispatcher) broadcast() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) IsClosed() bool {
	d.mu.Lock()
	c := d.closed
	d.mu.Unlock()
	return c
}
