package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/Hexrotor/f2p/internal/holepunch"
	"github.com/Hexrotor/f2p/internal/message"
	"github.com/Hexrotor/f2p/internal/utils"
	"github.com/libp2p/go-libp2p/core/network"
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

	c.ResetBackoff()

	// Check if connection is relay-only (Limited).
	// libp2p typically connects via relay first, then DCUtR upgrades to direct.
	// Wait a few seconds for DCUtR before falling back to custom hole punching.
	isRelay := c.isRelayConnection(peerID)
	if isRelay {
		slog.Info("Connection is relay-only, waiting for libp2p direct connection upgrade...")
		if c.waitForDirectConnection(peerID, 8*time.Second) {
			slog.Info("Direct connection established via libp2p DCUtR")
			protoCtx, protoCancel := context.WithTimeout(c.ctx, 10*time.Second)
			defer protoCancel()
			return c.setupLibp2pProtocol(protoCtx, peerID)
		}

		slog.Info("Still relay-only after waiting, attempting custom hole punch")
		if err := c.attemptHolePunch(peerID); err != nil {
			slog.Warn("Hole punch failed", "error", err)
			return fmt.Errorf("hole punch failed (relay insufficient for sustained operation): %w", err)
		}
		// Hole punch succeeded → use QUIC for everything
		return c.setupQUICProtocol(peerID)
	}

	slog.Info("Direct connection established, using libp2p protocol")
	// Direct connection path (existing flow)
	return c.setupLibp2pProtocol(connCtx, peerID)
}

// isRelayConnection checks if all connections to the peer are relay (Limited).
func (c *Client) isRelayConnection(peerID peer.ID) bool {
	conns := c.host.Network().ConnsToPeer(peerID)
	if len(conns) == 0 {
		return false
	}
	for _, conn := range conns {
		if !conn.Stat().Limited {
			return false // has at least one direct connection
		}
	}
	return true
}

// waitForDirectConnection polls for a non-relay connection to appear,
// giving libp2p's DCUtR time to upgrade the relay connection to direct.
func (c *Client) waitForDirectConnection(peerID peer.ID, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-time.After(500 * time.Millisecond):
			if !c.isRelayConnection(peerID) {
				return true
			}
		case <-c.ctx.Done():
			return false
		}
	}
	return false
}

// attemptHolePunch performs NAT detection, signaling, and UDP hole punching.
func (c *Client) attemptHolePunch(peerID peer.ID) error {
	stunServers := c.config.Common.StunServers
	if len(stunServers) == 0 {
		stunServers = holepunch.DefaultSTUNServers
	}

	// 1. Use cached NAT info if available, otherwise detect via STUN
	c.directMu.RLock()
	natInfo := c.natInfo
	detected := c.natDetected
	c.directMu.RUnlock()

	if !detected || natInfo == nil {
		slog.Info("Detecting NAT type...")
		var err error
		natInfo, err = holepunch.DetectNAT(stunServers)
		if err != nil {
			return fmt.Errorf("NAT detection failed: %w", err)
		}
		c.directMu.Lock()
		c.natInfo = natInfo
		c.natDetected = true
		c.directMu.Unlock()
	} else {
		slog.Info("Using cached NAT info", "type", natInfo.Type,
			"public", fmt.Sprintf("%s:%d", natInfo.PublicIP, natInfo.PublicPort))
	}

	// 2. Signaling over relay
	sigCtx, sigCancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer sigCancel()

	serverNAT, sessionToken, method, tid, err := holepunch.ClientSignaling(sigCtx, c.host, peerID, natInfo)
	if err != nil {
		return fmt.Errorf("signaling failed: %w", err)
	}
	slog.Info("Signaling complete", "method", method, "server_nat", serverNAT.Type)

	// 3. Create punch socket if we're the Cone side
	var punchSock *net.UDPConn
	if natInfo.Type.IsCone() && method != holepunch.PunchSymToCone {
		// ConeToCone: need a dedicated socket
		punchSock, natInfo, err = holepunch.CreatePunchSocket(stunServers[0])
		if err != nil {
			return fmt.Errorf("create punch socket: %w", err)
		}
	} else if natInfo.Type.IsCone() && method == holepunch.PunchSymToCone {
		// I'm Cone, server is Sym: need a socket to send from
		punchSock, _, err = holepunch.CreatePunchSocket(stunServers[0])
		if err != nil {
			return fmt.Errorf("create punch socket: %w", err)
		}
	}

	// 4. Punch
	punched, err := holepunch.ExecutePunch(natInfo, serverNAT, method, tid, punchSock)
	if err != nil {
		if punchSock != nil {
			punchSock.Close()
		}
		return fmt.Errorf("hole punch failed: %w", err)
	}
	slog.Info("Hole punch succeeded", "local", punched.LocalAddr, "remote", punched.RemoteAddr)

	// 5. Establish QUIC on punched socket
	quicConn, err := holepunch.DirectDialQUIC(c.ctx, punched, sessionToken)
	if err != nil {
		punched.Conn.Close()
		return fmt.Errorf("QUIC establishment failed: %w", err)
	}

	c.directMu.Lock()
	c.directConn = quicConn
	c.directMu.Unlock()

	slog.Info("Direct QUIC connection established via hole punch")
	return nil
}

// setupQUICProtocol runs the full f2p protocol (auth, verify, services) on the direct QUIC connection.
func (c *Client) setupQUICProtocol(peerID peer.ID) error {
	c.directMu.RLock()
	quicConn := c.directConn
	c.directMu.RUnlock()

	if quicConn == nil {
		return fmt.Errorf("no direct QUIC connection")
	}

	// Open control stream on QUIC
	ctrlStream, err := quicConn.OpenStream()
	if err != nil {
		return fmt.Errorf("open QUIC control stream: %w", err)
	}
	// Write stream type prefix
	if _, err := ctrlStream.Write([]byte{holepunch.StreamTypeControl}); err != nil {
		ctrlStream.Close()
		return fmt.Errorf("write control stream type: %w", err)
	}

	slog.Info("Connected to server via direct QUIC")

	c.streamMutex.Lock()
	c.controlStream = nil // no libp2p control stream
	c.quicCtrlClose = ctrlStream
	c.controlMessager = message.NewMessager(ctrlStream, c.config, peerID)
	c.controlDispatcher = message.NewDispatcher(c.controlMessager)
	c.controlDispatcher.Start()
	c.streamMutex.Unlock()

	if err := c.authenticateWithServer(); err != nil {
		if errors.Is(err, utils.ErrPasswordInterrupted) {
			c.setShutdownReason("user interrupt (password input)")
			time.Sleep(1 * time.Second)
			c.cancel()
			return fmt.Errorf("authentication cancelled")
		}
		ctrlStream.Close()
		return fmt.Errorf("authentication failed: %v", err)
	}
	if err := c.verifyServiceConfiguration(); err != nil {
		ctrlStream.Close()
		c.setShutdownReason("service verification failed (configuration error)")
		fmt.Printf("Service verification failed - this should be your configuration issue!\nPlease check your client configuration and ensure services exist on server.\n")
		c.cancel()
		return fmt.Errorf("service verification failed: %v", err)
	}

	c.startServicesOnce.Do(func() { go c.startLocalServices() })

	go c.monitorControlStream()

	slog.Info("Client connected successfully (direct QUIC)")
	c.printClientInfo()
	return nil
}

// setupLibp2pProtocol is the original connection flow using libp2p streams.
func (c *Client) setupLibp2pProtocol(connCtx context.Context, peerID peer.ID) error {
	controlProto := protocol.ID(utils.ControlProtocol(c.config.Common.Protocol))
	controlStream, err := c.host.NewStream(connCtx, peerID, controlProto)
	if err != nil {
		return fmt.Errorf("failed to create control stream: %v", err)
	}

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
			time.Sleep(1 * time.Second)
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

// openDataStream opens a data stream, preferring direct QUIC if available.
func (c *Client) openDataStream(useZstd bool) (io.ReadWriteCloser, error) {
	c.directMu.RLock()
	quicConn := c.directConn
	c.directMu.RUnlock()

	if quicConn != nil {
		stream, err := quicConn.OpenStream()
		if err != nil {
			return nil, fmt.Errorf("open QUIC data stream: %w", err)
		}
		streamType := holepunch.StreamTypeData
		if useZstd {
			streamType = holepunch.StreamTypeDataZstd
		}
		if _, err := stream.Write([]byte{streamType}); err != nil {
			stream.Close()
			return nil, fmt.Errorf("write data stream type: %w", err)
		}
		return stream, nil
	}

	// Fall back to libp2p
	dataProto := protocol.ID(utils.DataProtocol(c.config.Common.Protocol))
	if useZstd {
		dataProto = protocol.ID(utils.DataProtocolZstd(c.config.Common.Protocol))
	}
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	// Use WithAllowLimitedConn for relay compatibility
	ctx = network.WithAllowLimitedConn(ctx, "f2p-data")
	return c.host.NewStream(ctx, c.serverPeerID, dataProto)
}

// isDirectMode returns true if using direct QUIC connection.
func (c *Client) isDirectMode() bool {
	c.directMu.RLock()
	defer c.directMu.RUnlock()
	return c.directConn != nil
}

// connectionInfo returns a string describing the connection type for logging.
func (c *Client) connectionInfo() string {
	if c.isDirectMode() {
		return "direct-quic"
	}
	conns := c.host.Network().ConnsToPeer(c.serverPeerID)
	for _, conn := range conns {
		addr := conn.RemoteMultiaddr().String()
		if strings.Contains(addr, "p2p-circuit") {
			return "relay"
		}
	}
	return "libp2p-direct"
}
