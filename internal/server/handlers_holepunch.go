package server

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/Hexrotor/f2p/internal/holepunch"
	"github.com/Hexrotor/f2p/internal/message"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/quic-go/quic-go"
)

// detectNATType runs STUN-based NAT detection and caches the result.
func (s *Server) detectNATType() {
	slog.Info("Detecting server NAT type...")

	stunServers := s.config.Common.StunServers
	if len(stunServers) == 0 {
		stunServers = holepunch.DefaultSTUNServers
	}

	natInfo, err := holepunch.DetectNAT(stunServers)
	if err != nil {
		slog.Warn("NAT detection failed, hole punching will be unavailable", "error", err)
		return
	}

	s.natInfoMu.Lock()
	s.natInfo = natInfo
	s.natDetected = true
	s.natInfoMu.Unlock()

	slog.Info("Server NAT detected",
		"type", natInfo.Type,
		"public", fmt.Sprintf("%s:%d", natInfo.PublicIP, natInfo.PublicPort))
}

// handleHolePunchSignaling handles incoming hole punch signaling requests from clients.
func (s *Server) handleHolePunchSignaling(stream network.Stream) {
	remotePeer := stream.Conn().RemotePeer()
	slog.Info("Hole punch signaling request", "client", remotePeer.ShortString())

	// Check NAT detection is ready
	s.natInfoMu.RLock()
	natDetected := s.natDetected
	natInfo := s.natInfo
	s.natInfoMu.RUnlock()

	if !natDetected || natInfo == nil {
		slog.Warn("NAT not yet detected, rejecting signaling request")
		stream.Reset()
		return
	}

	// Generate session token
	sessionToken, err := holepunch.GenerateSessionToken()
	if err != nil {
		slog.Error("Failed to generate session token", "error", err)
		stream.Reset()
		return
	}

	// Run signaling protocol
	stunServers := s.config.Common.StunServers
	if len(stunServers) == 0 {
		stunServers = holepunch.DefaultSTUNServers
	}

	err = holepunch.ServerSignalingHandler(
		stream,
		natInfo.Type,
		natInfo,
		stunServers[0],
		sessionToken,
		func(punchSock *net.UDPConn, myNAT *holepunch.NATInfo, clientNAT *holepunch.NATInfo, method holepunch.PunchMethod, tid uint32) error {
			// Start punch + QUIC goroutine
			go s.handlePunchAndQUIC(punchSock, myNAT, clientNAT, method, tid, sessionToken, remotePeer)
			return nil
		},
	)
	if err != nil {
		slog.Error("Signaling failed", "error", err, "client", remotePeer.ShortString())
	}
}

// handlePunchAndQUIC performs the punch, sets up QUIC, and handles the direct connection.
func (s *Server) handlePunchAndQUIC(
	punchSock *net.UDPConn,
	myNAT *holepunch.NATInfo,
	clientNAT *holepunch.NATInfo,
	method holepunch.PunchMethod,
	tid uint32,
	sessionToken []byte,
	clientPeer peer.ID,
) {
	// 1. Execute punch
	punched, err := holepunch.ExecutePunch(myNAT, clientNAT, method, tid, punchSock)
	if err != nil {
		slog.Error("Hole punch failed", "error", err, "client", clientPeer.ShortString())
		if punchSock != nil {
			punchSock.Close()
		}
		return
	}
	slog.Info("Hole punch succeeded (server side)",
		"local", punched.LocalAddr, "remote", punched.RemoteAddr, "client", clientPeer.ShortString())

	// 2. Start QUIC listener on punched socket
	_, ln, err := holepunch.DirectListenQUIC(punched)
	if err != nil {
		slog.Error("QUIC listen failed", "error", err, "client", clientPeer.ShortString())
		punched.Conn.Close()
		return
	}
	defer ln.Close()

	// 3. Accept and authenticate QUIC connection
	quicConn, err := holepunch.AcceptAndAuthQUIC(s.ctx, ln, sessionToken)
	if err != nil {
		slog.Error("QUIC auth failed", "error", err, "client", clientPeer.ShortString())
		return
	}
	slog.Info("Direct QUIC connection authenticated", "client", clientPeer.ShortString())

	// 4. Handle QUIC streams
	s.handleQUICConnection(quicConn, clientPeer)
}

// handleQUICConnection accepts streams from a direct QUIC connection and routes them.
func (s *Server) handleQUICConnection(conn *quic.Conn, clientPeer peer.ID) {
	defer conn.CloseWithError(0, "done")

	slog.Info("Handling direct QUIC connection", "client", clientPeer.ShortString())

	for {
		stream, err := conn.AcceptStream(s.ctx)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			slog.Debug("QUIC accept stream ended", "error", err, "client", clientPeer.ShortString())
			return
		}

		// Read stream type byte
		typeBuf := make([]byte, 1)
		stream.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(stream, typeBuf); err != nil {
			slog.Warn("Failed to read QUIC stream type", "error", err)
			stream.Close()
			continue
		}
		stream.SetReadDeadline(time.Time{}) // clear deadline

		switch typeBuf[0] {
		case holepunch.StreamTypeControl:
			go s.handleQUICControlStream(stream, clientPeer)
		case holepunch.StreamTypeData:
			go s.handleQUICDataStream(stream, clientPeer, false)
		case holepunch.StreamTypeDataZstd:
			go s.handleQUICDataStream(stream, clientPeer, true)
		default:
			slog.Warn("Unknown QUIC stream type", "type", typeBuf[0])
			stream.Close()
		}
	}
}

// handleQUICControlStream handles a control stream on the direct QUIC connection.
// It performs auth handshake and then monitors heartbeat, just like the libp2p control stream.
func (s *Server) handleQUICControlStream(stream *quic.Stream, clientPeer peer.ID) {
	slog.Info("New QUIC control stream", "client", clientPeer.ShortString())

	messager := message.NewMessager(stream, s.config, clientPeer)
	disp := message.NewDispatcher(messager)
	disp.Start()

	// Check if client is already authenticated (from a previous libp2p session)
	s.clientsMutex.RLock()
	_, exists := s.authenticatedClients[clientPeer]
	s.clientsMutex.RUnlock()

	if !exists {
		// Perform auth on this QUIC control stream
		if err := s.handleAuthOnControl(disp, messager); err != nil {
			slog.Warn("QUIC auth failed", "client", clientPeer.ShortString(), "error", err)
			disp.Close()
			stream.Close()
			return
		}
	} else {
		// Replace old session
		s.clientsMutex.Lock()
		old := s.authenticatedClients[clientPeer]
		if old != nil {
			delete(s.authenticatedClients, clientPeer)
		}
		s.clientsMutex.Unlock()
		if old != nil {
			if old.controlDispatcher != nil {
				old.controlDispatcher.Close()
			}
			if old.controlStream != nil {
				_ = old.controlStream.Close()
			}
		}
	}

	cs := &ClientSession{
		peerID:            clientPeer,
		controlStream:     nil, // no libp2p stream
		controlMessager:   messager,
		controlDispatcher: disp,
		authTime:          time.Now(),
		lastSeen:          time.Now(),
	}
	s.clientsMutex.Lock()
	s.authenticatedClients[clientPeer] = cs
	s.clientsMutex.Unlock()
	s.host.ConnManager().TagPeer(clientPeer, "authenticated-client", 100)

	// Monitor heartbeat (reuses existing logic)
	s.monitorControlStream(cs)
}

// handleQUICDataStream handles a data stream on the direct QUIC connection.
func (s *Server) handleQUICDataStream(stream *quic.Stream, clientPeer peer.ID, useZstd bool) {
	// Verify client is authenticated
	s.clientsMutex.RLock()
	_, exists := s.authenticatedClients[clientPeer]
	s.clientsMutex.RUnlock()

	if !exists {
		slog.Warn("QUIC data stream from unauthenticated client", "client", clientPeer.ShortString())
		stream.Close()
		return
	}

	// Reuse the same data handling logic as libp2p streams
	s.handleDataConnection(stream, clientPeer, useZstd)
}
