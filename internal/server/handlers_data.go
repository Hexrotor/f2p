package server

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/Hexrotor/f2p/internal/message"
	"github.com/Hexrotor/f2p/internal/utils"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func (s *Server) handleDataStream(stream network.Stream) {
	remotePeer := stream.Conn().RemotePeer()
	protoID := string(stream.Protocol())
	isZstd := strings.HasSuffix(protoID, "/data+zstd")

	s.clientsMutex.RLock()
	_, exists := s.authenticatedClients[remotePeer]
	s.clientsMutex.RUnlock()

	if !exists {
		slog.Warn("Data stream from unauthenticated client, resetting", "client", remotePeer.String())
		_ = stream.Reset()
		return
	}

	if isZstd {
		s.handleServiceStreamWithCompression(stream, remotePeer, true)
	} else {
		s.handleServiceStreamWithCompression(stream, remotePeer, false)
	}
}

func (s *Server) handleServiceStreamWithCompression(stream network.Stream, clientPeer peer.ID, useZstd bool) error {
	var rw io.ReadWriteCloser = stream
	if useZstd {
		z, err := utils.NewZstdDuplex(stream)
		if err != nil {
			slog.Error("Failed to init zstd on data stream", "error", err)
			_ = stream.Reset()
			return err
		}
		z.SetInfo("server", "", clientPeer.ShortString(), "") // protocol & service later
		rw = z
	}

	messager := message.NewMessager(rw, s.config, clientPeer)
	var serviceName, protocolType string
	targetService, err := s.handleServiceHandshake(messager, &serviceName, &protocolType)
	if err != nil {
		return fmt.Errorf("handshake failed: %v", err)
	}

	clientID := clientPeer.String()
	slog.Info("Forwarding request", "client", clientID, "service", serviceName, "protocol", protocolType, "target", targetService.Target, "compress", useZstd)

	if dz, ok := rw.(interface {
		SetFields(string, string, string, string) *utils.ZstdDuplex
	}); ok {
		// Only service & protocol now (side/peer already set)
		dz.SetFields("", serviceName, "", protocolType)
	}

	if err := s.connectAndProxy(rw, protocolType, targetService.Target); err != nil {
		return fmt.Errorf("failed to connect to target %s: %v", targetService.Target, err)
	}
	return nil
}
