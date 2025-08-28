package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Hexrotor/f2p/internal/message"
	"github.com/Hexrotor/f2p/internal/utils"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
)

func (c *Client) connectToServer(peerID peer.ID) error {
	slog.Info("Querying", "server_id", peerID.ShortString())
	peerInfo, err := c.dht.FindPeer(c.ctx, peerID)
	if err != nil {
		slog.Error("DHT find peer failed", "server_id", peerID.ShortString(), "error", err)
		return err
	}

	slog.Info("FindPeer", "peer_id", peerID.ShortString(), "addresses", peerInfo.Addrs)

	connCtx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()

	if swrm, ok := c.host.Network().(*swarm.Swarm); ok {
		swrm.Backoff().Clear(peerID)
	}

	err = c.host.Connect(connCtx, peerInfo)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %v", err)
	}

	controlProto := protocol.ID(utils.ControlProtocol(c.config.Common.Protocol))
	controlStream, err := c.host.NewStream(connCtx, peerID, controlProto)
	if err != nil {
		return fmt.Errorf("failed to create control stream: %v", err)
	}

	c.ResetBackoff()

	slog.Info("Connected to server", "addr", controlStream.Conn().RemoteMultiaddr())

	c.streamMutex.Lock()
	c.controlStream = controlStream
	c.controlMessager = message.NewMessager(controlStream, c.config, peerID)
	c.controlDispatcher = message.NewDispatcher(c.controlMessager)
	c.controlDispatcher.Start()
	c.streamMutex.Unlock()

	if err := c.authenticateWithServer(); err != nil {
		if errors.Is(err, utils.ErrPasswordInterrupted) {
			c.setShutdownReason("user interrupt (password input)")
			c.cancel()
			return fmt.Errorf("authentication cancelled")
		}
		controlStream.Close()
		return fmt.Errorf("authentication failed: %v", err)
	}
	if err := c.verifyServiceConfiguration(); err != nil {
		controlStream.Close()
		c.setShutdownReason("service verification failed (configuration error)")
		fmt.Printf("Service verification failed - this should be your configuration issue!\nPlease check your client configuration and ensure services exist on server.\n")
		c.cancel()
		return fmt.Errorf("service verification failed: %v", err)
	}

	c.startServicesOnce.Do(func() { go c.startLocalServices() })

	go c.monitorControlStream()

	slog.Info("Client connected successfully")
	c.printClientInfo()

	return nil
}
