package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	pb "github.com/Hexrotor/f2p/proto"
)

func (c *Client) monitorControlStream() {
	serverShutdownReceived := false
	defer func() {
		if c.stopping.Load() {
			// intentional stop; suppress reconnection
			return
		}
		if serverShutdownReceived {
			slog.Info("Control stream disconnected due to server shutdown, waiting 10s before reconnecting")
			time.Sleep(10 * time.Second)
		} else {
			slog.Info("Control stream disconnected, restarting connection manager")
		}
		go c.startConnectionManager()
	}()

	// Check that we have a control stream (either libp2p or QUIC)
	c.streamMutex.RLock()
	controlStream := c.controlStream
	quicCtrl := c.quicCtrlClose
	c.streamMutex.RUnlock()

	if controlStream == nil && quicCtrl == nil {
		return
	}

	lastHeartbeat := time.Now()
	// Expect server ping every 10s; treat >25s silence as failure (includes some margin)
	heartbeatTimeout := 25 * time.Second
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			if time.Since(lastHeartbeat) > heartbeatTimeout {
				slog.Warn("Heartbeat timeout, closing control stream to reconnect", "server", c.serverPeerID.ShortString())
				c.closeControlStream()
				return
			}
			c.streamMutex.RLock()
			disp := c.controlDispatcher
			m := c.controlMessager
			c.streamMutex.RUnlock()
			if disp == nil || m == nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			msg, err := disp.WaitFor(ctx, pb.MessageType_HEARTBEAT, pb.MessageType_SERVER_SHUTDOWN, pb.MessageType_SERVICE_RESPONSE)
			cancel()
			if err != nil {
				if ctx.Err() != nil { // timeout; loop again
					continue
				}
				if err != io.EOF && !strings.Contains(err.Error(), "closed") {
					slog.Error("Control stream dispatcher error", "error", err)
				}
				c.closeControlStream()
				return
			}
			if msg.IsShutdownMessage() {
				slog.Info("Server shutdown notification received", "server", c.serverPeerID.ShortString(), "message", msg.Message)
				fmt.Printf("Server shutdown notification received [Server: %s]\n", c.serverPeerID.ShortString())
				fmt.Printf("Message: %s\n", msg.Message)
				serverShutdownReceived = true
				return
			}
			if msg.Type == pb.MessageType_HEARTBEAT {
				lastHeartbeat = time.Now()
				// server ping -> client pong
				if msg.Message == "ping" {
					if err := m.SendHeartbeatAck(); err != nil {
						slog.Error("Failed to send heartbeat ack", "error", err, "server", c.serverPeerID.ShortString())
						c.closeControlStream()
						return
					}
				}
				continue
			}
			slog.Info("Received control message", "type", msg.Type, "message", msg.Message)
		}
	}
}

// closeControlStream closes whichever control stream is active (libp2p or QUIC).
// Called by monitorControlStream when a disconnection is detected.
func (c *Client) closeControlStream() {
	c.streamMutex.RLock()
	cs := c.controlStream
	qc := c.quicCtrlClose
	c.streamMutex.RUnlock()

	if cs != nil {
		_ = cs.Close()
	}
	if qc != nil {
		_ = qc.Close()
	}

	// Close and nil out the QUIC connection if in direct mode
	if c.cleanupDirectConn("control stream closed") {
		// was direct QUIC, already cleaned up
	} else {
		c.host.Network().ClosePeer(c.serverPeerID)
	}
}

// cleanupPreviousConnection comprehensively clears ALL stale connection state
// (both libp2p and QUIC) at the start of a new connection cycle. This ensures
// openDataStream, monitorControlStream, etc. don't use dead connections.
func (c *Client) cleanupPreviousConnection() {
	// 1. Close dispatcher first (stops reading from control stream)
	c.streamMutex.Lock()
	if c.controlDispatcher != nil {
		c.controlDispatcher.Close()
		c.controlDispatcher = nil
	}
	c.controlMessager = nil

	// 2. Close libp2p control stream
	cs := c.controlStream
	c.controlStream = nil

	// 3. Close QUIC control stream closer
	qc := c.quicCtrlClose
	c.quicCtrlClose = nil
	c.streamMutex.Unlock()

	if cs != nil {
		_ = cs.Close()
	}
	if qc != nil {
		_ = qc.Close()
	}

	// 4. Close and nil out direct QUIC connection
	c.cleanupDirectConn("new connection cycle")

	// 5. Close any stale libp2p peer connections
	c.host.Network().ClosePeer(c.serverPeerID)

	slog.Debug("Cleaned up previous connection state")
}

// cleanupDirectConn closes the direct QUIC connection (if any) and nils it out
// so that openDataStream falls back to libp2p on the next connection cycle.
// Returns true if a QUIC connection was cleaned up.
func (c *Client) cleanupDirectConn(reason string) bool {
	c.directMu.Lock()
	dc := c.directConn
	c.directConn = nil
	c.directMu.Unlock()

	if dc != nil {
		dc.CloseWithError(0, reason)
		slog.Debug("Cleaned up stale direct QUIC connection", "reason", reason)
		return true
	}
	return false
}
