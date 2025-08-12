package client

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Hexrotor/f2p/internal/utils"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
)

func (c *Client) connectToServer(peerID peer.ID) error {
	slog.Info("Querying", "server_id", peerID.ShortString())
	peerInfo, err := c.dht.FindPeer(c.ctx, peerID)
	if err != nil {
		slog.Error("DHT find peer failed", "server_id", peerID.ShortString(), "error", err)
		return err
	}
	c.host.Peerstore().AddAddrs(peerID, peerInfo.Addrs, peerstore.TempAddrTTL)
	slog.Info("Peerstore", "peerID", peerID.ShortString(), "addresses", c.host.Peerstore().Addrs(peerID))

	connCtx, cancel := context.WithTimeout(c.ctx, 60*time.Second)
	defer cancel()

	controlProto := protocol.ID(utils.ControlProtocol(c.config.Common.Protocol))
	controlStream, err := c.host.NewStream(connCtx, peerID, controlProto)
	if err != nil {
		return fmt.Errorf("failed to create control stream: %v", err)
	}

	c.ResetBackoff()

	slog.Info("Connected to server", "addr", controlStream.Conn().RemoteMultiaddr())

	c.streamMutex.Lock()
	c.controlStream = controlStream
	c.streamMutex.Unlock()

	if err := c.authenticateWithServer(controlStream, peerID); err != nil {
		controlStream.Close()
		return fmt.Errorf("authentication failed: %v", err)
	}

	if err := c.verifyServiceConfiguration(); err != nil {
		controlStream.Close()
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
