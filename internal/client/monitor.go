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
		if serverShutdownReceived {
			slog.Info("Control stream disconnected due to server shutdown, waiting 10s before reconnecting")
			time.Sleep(10 * time.Second)
		} else {
			slog.Info("Control stream disconnected, restarting connection manager")
		}
		go c.startConnectionManager()
	}()

	c.streamMutex.RLock()
	controlStream := c.controlStream
	c.streamMutex.RUnlock()

	if controlStream == nil {
		return
	}

	lastHeartbeat := time.Now()
	heartbeatTimeout := 70 * time.Second
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			if time.Since(lastHeartbeat) > heartbeatTimeout {
				slog.Warn("Heartbeat timeout, closing control stream to reconnect", "server", c.serverPeerID.ShortString())
				c.streamMutex.RLock()
				cs := c.controlStream
				c.streamMutex.RUnlock()
				if cs != nil {
					_ = cs.Close()
				}
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
				c.streamMutex.RLock()
				cs := c.controlStream
				c.streamMutex.RUnlock()
				if cs != nil {
					_ = cs.Close()
				}
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
						c.streamMutex.RLock()
						cs := c.controlStream
						c.streamMutex.RUnlock()
						if cs != nil {
							_ = cs.Close()
						}
						return
					}
				}
				continue
			}
			slog.Info("Received control message", "type", msg.Type, "message", msg.Message)
		}
	}
}
