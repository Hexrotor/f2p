package message

import (
	"fmt"
	"io"
	"time"

	"github.com/Hexrotor/f2p/internal/config"
	pb "github.com/Hexrotor/f2p/proto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

type Messager struct {
	conn   io.ReadWriteCloser
	config *config.Config
	peerID peer.ID
}

// internal constructor used by API wrapper
func createMessager(conn io.ReadWriteCloser, cfg *config.Config, peerID peer.ID) *Messager {
	return &Messager{conn: conn, config: cfg, peerID: peerID}
}

// getPeerID returns the peer id (used by API wrapper)
func (h *Messager) getPeerID() peer.ID { return h.peerID }

// internal safety limits for handshake/control frames
const maxProtoMsgSize = 4 << 20 // 4 MiB

// Optional deadline capabilities (compatible with libp2p streams that implement deadlines)
type readDeadline interface{ SetReadDeadline(time.Time) error }
type writeDeadline interface{ SetWriteDeadline(time.Time) error }

func writeFull(w io.Writer, p []byte) error {
	for off := 0; off < len(p); {
		n, err := w.Write(p[off:])
		if n > 0 {
			off += n
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *Messager) sendMessage(msg *pb.UnifiedMessage) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data) > maxProtoMsgSize {
		return fmt.Errorf("message too large: %d > %d", len(data), maxProtoMsgSize)
	}
	msgLen := uint32(len(data))
	lenBytes := []byte{byte(msgLen >> 24), byte(msgLen >> 16), byte(msgLen >> 8), byte(msgLen)}
	if d, ok := h.conn.(writeDeadline); ok {
		_ = d.SetWriteDeadline(time.Now().Add(30 * time.Second))
		defer d.SetWriteDeadline(time.Time{})
	}
	if err := writeFull(h.conn, lenBytes); err != nil {
		return err
	}
	return writeFull(h.conn, data)
}

// receiveMessage reads a single length-prefixed protobuf message.
// If timeout > 0 and the connection supports deadlines, a read deadline is applied for the duration.
func (h *Messager) receiveMessage(timeout time.Duration) (*pb.UnifiedMessage, error) {
	if d, ok := h.conn.(readDeadline); ok && timeout > 0 {
		_ = d.SetReadDeadline(time.Now().Add(timeout))
		defer d.SetReadDeadline(time.Time{})
	}
	lenBytes := make([]byte, 4)
	if _, err := io.ReadFull(h.conn, lenBytes); err != nil {
		return nil, err
	}
	msgLen := uint32(lenBytes[0])<<24 | uint32(lenBytes[1])<<16 | uint32(lenBytes[2])<<8 | uint32(lenBytes[3])
	if msgLen > maxProtoMsgSize {
		return nil, fmt.Errorf("invalid message size: %d", msgLen)
	}
	if msgLen == 0 {
		return &pb.UnifiedMessage{}, nil
	}
	data := make([]byte, msgLen)
	if _, err := io.ReadFull(h.conn, data); err != nil {
		return nil, err
	}
	var msg pb.UnifiedMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
