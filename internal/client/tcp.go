package client

import (
	"log/slog"
	"net"

	"github.com/Hexrotor/f2p/internal/config"
)

func (c *Client) startLocalServices() {
	slog.Info("Starting local services")
	for _, localService := range c.localServices {
		if !localService.Enabled {
			slog.Debug("Skipping disabled local service", "service", localService.Name)
			continue
		}
		go c.startLocalService(localService)
	}
}

func (c *Client) startLocalService(localService *config.LocalServiceConfig) {
	slog.Info("Starting local service", "name", localService.Name, "protocols", localService.Protocol, "address", localService.Local)

	for _, protocolType := range localService.Protocol {
		switch protocolType {
		case "tcp":
			go c.startTCPService(localService)
		case "udp":
			go c.startUDPService(localService)
		default:
			slog.Error("Unsupported protocol", "protocol", protocolType)
		}
	}
}

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
