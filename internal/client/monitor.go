package client

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Hexrotor/f2p/internal/message"
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

	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-heartbeatTicker.C:
			messager := message.NewMessager(controlStream, c.config, c.serverPeerID)
			if err := messager.SendHeartbeat(); err != nil {
				slog.Error("Failed to send heartbeat", "error", err, "server", c.serverPeerID.ShortString())
				_ = controlStream.Close()
				time.Sleep(500 * time.Millisecond)
				return
			}
		default:
			messager := message.NewMessager(controlStream, c.config, c.serverPeerID)
			msg, err := messager.ReceiveMessage()

			if err != nil {
				if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "timeout") {
					continue
				}

				if !strings.Contains(err.Error(), "timeout") && err != io.EOF {
					slog.Error("Control stream error", "error", err, "server", c.serverPeerID.ShortString())
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

			switch msg.Type {
			case pb.MessageType_HEARTBEAT:
				if err := messager.SendHeartbeat(); err != nil {
					slog.Error("Failed to respond to heartbeat", "error", err, "server", c.serverPeerID.ShortString())
					c.streamMutex.RLock()
					cs := c.controlStream
					c.streamMutex.RUnlock()
					if cs != nil {
						_ = cs.Close()
					}
					return
				}
			default:
				slog.Info("Received control message", "type", msg.Type, "message", msg.Message)
			}
		}
	}
}
