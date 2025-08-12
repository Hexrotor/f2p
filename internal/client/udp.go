package client

import (
	"log/slog"
	"net"

	"github.com/Hexrotor/f2p/internal/config"
	udp "github.com/pion/udp/v2"
)

func (c *Client) startUDPService(localService *config.LocalServiceConfig) {
	// 使用 pion/udp 的“伪连接”监听，让 UDP 也能像 TCP 一样 Accept/处理
	addr, err := net.ResolveUDPAddr("udp", localService.Local)
	if err != nil {
		slog.Error("Failed to resolve UDP address", "address", localService.Local, "error", err)
		return
	}
	ln, err := udp.Listen("udp", &net.UDPAddr{IP: addr.IP, Port: addr.Port})
	if err != nil {
		slog.Error("Failed to bind UDP", "address", localService.Local, "error", err)
		return
	}
	defer ln.Close()

	slog.Info("UDP service binding", "service", localService.Name, "address", localService.Local)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-c.ctx.Done():
				return
			default:
			}
			slog.Error("UDP accept error", "error", err)
			continue
		}

		go c.handleLocalConnForProtocol(conn, localService, "udp")
	}
}
