package client

import (
	"log/slog"
	"net"

	"github.com/Hexrotor/f2p/internal/config"
)

func (c *Client) startTCPService(localService *config.LocalServiceConfig) {
	listener, err := net.Listen("tcp", localService.Local)
	if err != nil {
		slog.Error("Failed to listen on TCP", "address", localService.Local, "error", err)
		return
	}
	defer listener.Close()

	slog.Info("TCP service listening", "service", localService.Name, "address", localService.Local)

	for {
		localConn, err := listener.Accept()
		if err != nil {
			slog.Error("TCP accept error", "error", err)
			continue
		}

		go c.handleLocalConnForProtocol(localConn, localService, "tcp")
	}
}
