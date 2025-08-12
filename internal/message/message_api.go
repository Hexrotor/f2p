package message

import (
	"io"
	"time"

	"github.com/Hexrotor/f2p/internal/config"
	pb "github.com/Hexrotor/f2p/proto"
	"github.com/libp2p/go-libp2p/core/peer"
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
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_SERVER_SHUTDOWN, Message: "Server is shutting down gracefully"})
}

// SendClientShutdownNotification sends a client shutdown notice.
func (h *Messager) SendClientShutdownNotification() error {
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_CLIENT_SHUTDOWN, Message: "Client is shutting down gracefully"})
}

// SendHeartbeat sends a heartbeat control message.
func (h *Messager) SendHeartbeat() error {
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_HEARTBEAT})
}

// SendServiceRequest requests a specific service/protocol (optionally with password).
func (h *Messager) SendServiceRequest(name, protocol, pwd string) error {
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_SERVICE_REQUEST, ServiceName: name, Protocol: protocol, ServicePassword: pwd})
}

// SendServiceResponse replies to a service request.
func (h *Messager) SendServiceResponse(ok bool, code pb.ErrorCode, msg string, data map[string]string) error {
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_SERVICE_RESPONSE, Success: ok, ErrorCode: code, Message: msg, Data: data})
}

// SendPasswordRequired indicates whether password is required.
func (h *Messager) SendPasswordRequired(require bool) error {
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_PASSWORD_REQUIRED, RequirePassword: require})
}

// SendAuthRequest sends an authentication request with password.
func (h *Messager) SendAuthRequest(password string) error {
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_AUTH_REQUEST, Password: password})
}

// SendAuthResponse replies to an authentication request.
func (h *Messager) SendAuthResponse(ok bool, msg string) error {
	return h.sendMessage(&pb.UnifiedMessage{Type: pb.MessageType_AUTH_RESPONSE, Success: ok, Message: msg})
}

// ReceiveMessage reads a message with default timeout.
func (h *Messager) ReceiveMessage() (*pb.UnifiedMessage, error) {
	return h.receiveMessage(30 * time.Second)
}

// ReceiveMessageWithTimeout reads a message with a specified timeout.
func (h *Messager) ReceiveMessageWithTimeout(timeout time.Duration) (*pb.UnifiedMessage, error) {
	return h.receiveMessage(timeout)
}
