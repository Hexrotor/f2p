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

	// Also close the QUIC connection if in direct mode
	c.directMu.RLock()
	dc := c.directConn
	c.directMu.RUnlock()
	if dc != nil {
		dc.CloseWithError(0, "control stream closed")
	} else {
		c.host.Network().ClosePeer(c.serverPeerID)
	}
}
