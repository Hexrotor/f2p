package client

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/Hexrotor/f2p/internal/config"
	"github.com/Hexrotor/f2p/internal/utils"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// handleLocalConnForProtocol unifies TCP/UDP per-connection flow: zstd decision → New data stream → SERVICE_REQUEST → proxy
func (c *Client) handleLocalConnForProtocol(local io.ReadWriteCloser, localService *config.LocalServiceConfig, protocolType string) {
	defer local.Close()

	// Compression decision (cached by service)
	useZstd := false
	c.compMutex.RLock()
	if v, ok := c.serviceCompress[localService.Name]; ok {
		useZstd = v
	}
	c.compMutex.RUnlock()

	dataProto := protocol.ID(utils.DataProtocol(c.config.Common.Protocol))
	if useZstd {
		dataProto = protocol.ID(utils.DataProtocolZstd(c.config.Common.Protocol))
	}

	// Create data stream to server
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	serverStream, err := c.host.NewStream(ctx, c.serverPeerID, dataProto)
	cancel()
	if err != nil {
		slog.Error("Failed to create data stream to server", "error", err, "server", c.serverPeerID.ShortString())
		return
	}

	var rw io.ReadWriteCloser = serverStream
	if useZstd {
		z, err := utils.NewZstdDuplex(serverStream)
		if err != nil {
			slog.Error("Failed to init zstd on client data stream", "error", err)
			serverStream.Close()
			return
		}
		z.SetMeta("client", localService.Name, c.serverPeerID.ShortString())
		rw = z
	}

	// Use checkServiceAvailability for service handshake
	if _, err := c.checkServiceAvailability(rw, c.serverPeerID, localService.Name, protocolType, localService.Password); err != nil {
		slog.Error("Check failed for service", "service", localService.Name, "protocol", protocolType, "error", err, "server", c.serverPeerID.ShortString())
		rw.Close()
		return
	}

	slog.Debug("Proxying connection", "service", localService.Name, "protocol", protocolType, "zstd", useZstd)
	c.proxyConnection(local, rw)
}
