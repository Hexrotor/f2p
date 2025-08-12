package server

import (
	"io"
	"net"
	"time"

	"github.com/Hexrotor/f2p/internal/utils"
)

func (s *Server) proxyConnection(conn1, conn2 io.ReadWriteCloser) {
	const idleTimeout = 60 * time.Second
	utils.PipeBothWithIdle(conn1, conn2, idleTimeout)
}

// connectAndProxy dials target using protocolType ("tcp" or "udp") and proxies rw <-> targetConn.
func (s *Server) connectAndProxy(rw io.ReadWriteCloser, protocolType string, target string) error {
	var (
		targetConn net.Conn
		err        error
	)
	dialTimeout := 10 * time.Second
	switch protocolType {
	case "tcp":
		targetConn, err = net.DialTimeout("tcp", target, dialTimeout)
	case "udp":
		// connected UDP socket
		var raddr *net.UDPAddr
		raddr, err = net.ResolveUDPAddr("udp", target)
		if err == nil {
			targetConn, err = net.DialUDP("udp", nil, raddr)
		}
	default:
		return nil
	}
	if err != nil {
		return err
	}
	s.proxyConnection(rw, targetConn)
	return nil
}
