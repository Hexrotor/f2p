package utils

import (
	"io"
	"sync/atomic"
	"time"
)

// PipeBothWithIdle copies data in both directions between a and b.
// It closes both sides when any direction finishes or when idle timeout elapses.
func PipeBothWithIdle(a, b io.ReadWriteCloser, idle time.Duration) {
	var lastSeen atomic.Int64
	lastSeen.Store(time.Now().UnixNano())
	touch := func() { lastSeen.Store(time.Now().UnixNano()) }

	done := make(chan struct{}, 2)
	stop := make(chan struct{})

	// Idle watcher, necessary for clean UDP connections
	go func() {
		if idle <= 0 {
			return
		}
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if time.Since(time.Unix(0, lastSeen.Load())) > idle {
					_ = a.Close()
					_ = b.Close()
					return
				}
			case <-stop:
				return
			}
		}
	}()

	copyLoop := func(dst, src io.ReadWriteCloser) {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				touch()
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
				touch()
			}
			if rerr != nil {
				break
			}
		}
		done <- struct{}{}
	}

	go copyLoop(a, b)
	go copyLoop(b, a)

	<-done
	close(stop)
	_ = a.Close()
	_ = b.Close()
}
