package message

import (
	"fmt"
	"io"
	"time"

	"github.com/Hexrotor/f2p/internal/config"
	pb "github.com/Hexrotor/f2p/proto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	defaultControlTTL = 30 * time.Second
)

// API for control messages and convenience handshakes.

// NewMessager constructs a handshake instance (exported API wrapper).
func NewMessager(conn io.ReadWriteCloser, cfg *config.Config, peerID peer.ID) *Messager {
	return createMessager(conn, cfg, peerID)
}

// PeerID returns the remote peer id (exported API wrapper).
func (h *Messager) PeerID() peer.ID { return h.getPeerID() }

// SendServerShutdownNotification sends a server shutdown notice.
func (h *Messager) SendServerShutdownNotification() error {
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_SERVER_SHUTDOWN, Message: "Server is shutting down gracefully"}, defaultControlTTL)
}

// SendClientShutdownNotification sends a client shutdown notice.
func (h *Messager) SendClientShutdownNotification() error {
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_CLIENT_SHUTDOWN, Message: "Client is shutting down gracefully"}, defaultControlTTL)
}

// SendHeartbeat sends a heartbeat control message.
func (h *Messager) SendHeartbeat() error { // 作为 ping (Server -> Client)
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_HEARTBEAT, Message: "ping"}, 10*time.Second)
}

func (h *Messager) SendHeartbeatAck() error { // 作为 pong (Client -> Server)
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_HEARTBEAT, Message: "pong"}, 10*time.Second)
}

// SendServiceRequest requests a specific service/protocol (optionally with password).
func (h *Messager) SendServiceRequest(name, protocol, pwd string) error {
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_SERVICE_REQUEST, ServiceName: name, Protocol: protocol, ServicePassword: pwd}, defaultControlTTL)
}

// SendServiceResponse replies to a service request.
func (h *Messager) SendServiceResponse(ok bool, code pb.ErrorCode, msg string, data map[string]string) error {
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_SERVICE_RESPONSE, Success: ok, ErrorCode: code, Message: msg, Data: data}, defaultControlTTL)
}

// SendPasswordRequired indicates whether password is required.
func (h *Messager) SendPasswordRequired(require bool) error {
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_PASSWORD_REQUIRED, RequirePassword: require}, defaultControlTTL)
}

// SendAuthRequest sends an authentication request with password.
func (h *Messager) SendAuthRequest(password string) error {
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_AUTH_REQUEST, Password: password}, defaultControlTTL)
}

// SendAuthResponse replies to an authentication request.
func (h *Messager) SendAuthResponse(ok bool, msg string) error {
	return h.SendWithTTL(&pb.UnifiedMessage{Type: pb.MessageType_AUTH_RESPONSE, Success: ok, Message: msg}, defaultControlTTL)
}

// TTL in dispatcher
func (h *Messager) SendWithTTL(msg *pb.UnifiedMessage, ttl time.Duration) error {
	const (
		defaultTTL = 30 * time.Second
		maxTTL     = 60 * time.Second
		minTTL     = 1 * time.Second
	)
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl < minTTL { // 强制最小 1s
		ttl = minTTL
	}
	if ttl > maxTTL {
		return fmt.Errorf("ttl exceeds max 60s: %v", ttl)
	}
	msg.TtlMs = uint32(ttl / time.Millisecond)
	// 保护：如果换算为0（极短）仍强制最小1ms
	if msg.TtlMs == 0 {
		msg.TtlMs = uint32(defaultTTL / time.Millisecond)
	}
	return h.sendMessage(msg)
}

// ReceiveOne 读取单个 protobuf 消息（仅用于数据流一次性握手，避免后台持续读取破坏后续原始字节）。
// timeout>0 时若底层支持 deadline 则设置读超时。
func (h *Messager) ReceiveOne(timeout time.Duration) (*pb.UnifiedMessage, error) {
	return h.receiveMessage(timeout)
}
