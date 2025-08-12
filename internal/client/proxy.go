package client

import (
	"io"
	"time"

	"github.com/Hexrotor/f2p/internal/utils"
)

func (c *Client) proxyConnection(conn1, conn2 io.ReadWriteCloser) {
	const idleTimeout = 60 * time.Second
	utils.PipeBothWithIdle(conn1, conn2, idleTimeout)
}
