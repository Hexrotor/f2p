// Package message implements a concurrency‑safe Dispatcher that continuously pulls UnifiedMessage
// instances from an underlying Messager, buckets them by pb.MessageType, and lets callers wait
// (blocking with context) for the first non‑expired message among one or more desired types.
// Each enqueued message is checked against its ttl_ms (with a minimum effective TTL of 1s);
// expired messages are dropped and counted per type (droppedExpired). WaitFor returns as soon
// as a valid message is available, propagates any receive error from the producer goroutine,
// or exits on context cancellation or dispatcher closure. A lightweight notification channel
// minimizes wake‑ups (best‑effort broadcast). The Dispatcher also supports: graceful Close()
// (unblocking waiters), length inspection (LenType), draining a type queue (DrainType), and
// a convenience WaitOne with timeout. All internal state is protected by a mutex.
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
	droppedExpired map[pb.MessageType]uint64
	notify         chan struct{}
}

type entry struct {
	msg       *pb.UnifiedMessage
	enqueueAt time.Time
}

func NewDispatcher(m *Messager) *Dispatcher {
	return &Dispatcher{m: m, queues: make(map[pb.MessageType][]entry), droppedExpired: make(map[pb.MessageType]uint64), notify: make(chan struct{}, 1)}
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
			d.mu.Lock()
			d.queues[msg.Type] = append(d.queues[msg.Type], entry{msg: msg, enqueueAt: time.Now()})
			d.mu.Unlock()
			d.broadcast()
		}
	}()
}

func (d *Dispatcher) WaitFor(ctx context.Context, types ...pb.MessageType) (*pb.UnifiedMessage, error) {
	typeSet := make(map[pb.MessageType]struct{}, len(types))
	for _, t := range types {
		typeSet[t] = struct{}{}
	}

	for {
		d.mu.Lock()
		for t := range typeSet {
			q := d.queues[t]
			if len(q) > 0 {
				for len(q) > 0 {
					e := q[0]
					expired := false
					ttlMs := e.msg.GetTtlMs()
					effectiveTTL := time.Duration(ttlMs) * time.Millisecond
					if effectiveTTL < time.Second {
						effectiveTTL = time.Second
					}
					if time.Since(e.enqueueAt) > effectiveTTL {
						expired = true
					}
					q = q[1:]
					if expired {
						d.droppedExpired[t]++
						continue
					}
					d.queues[t] = q
					d.mu.Unlock()
					return e.msg, nil
				}
				d.queues[t] = q
			}
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
			continue
		}
	}
}

func (d *Dispatcher) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.mu.Unlock()
	d.broadcast()
}

func (d *Dispatcher) broadcast() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}
func (d *Dispatcher) IsClosed() bool { d.mu.Lock(); c := d.closed; d.mu.Unlock(); return c }

func (d *Dispatcher) LenType(t pb.MessageType) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queues[t])
}
// DrainType atomically extracts and returns all queued *pb.UnifiedMessage entries for
// the specified message type t, preserving their current order, and empties (clears)
// the underlying queue for that type. It is safe for concurrent use: the dispatcher’s
// mutex is held for the duration of the drain to prevent races. After a successful
// call, the queue for t is reset (set to nil), so subsequent calls will return an
// empty slice until new messages are enqueued. If no messages are present, it returns
// an empty (non-nil) slice. This is typically used to batch-process and flush all
// pending messages of a given type efficiently.
func (d *Dispatcher) DrainType(t pb.MessageType) []*pb.UnifiedMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	ent := d.queues[t]
	d.queues[t] = nil
	out := make([]*pb.UnifiedMessage, 0, len(ent))
	for _, e := range ent {
		out = append(out, e.msg)
	}
	return out
}

func (d *Dispatcher) WaitOne(timeout time.Duration, t pb.MessageType) (*pb.UnifiedMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.WaitFor(ctx, t)
}
