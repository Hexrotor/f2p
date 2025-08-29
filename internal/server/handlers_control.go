package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Hexrotor/f2p/internal/config"
	"github.com/Hexrotor/f2p/internal/message"
	pb "github.com/Hexrotor/f2p/proto"
	"github.com/libp2p/go-libp2p/core/network"
)

func (s *Server) handleControlStream(stream network.Stream) {
	remotePeer := stream.Conn().RemotePeer()
	slog.Info("New control stream", "client", remotePeer.String())

	s.clientsMutex.RLock()
	_, exists := s.authenticatedClients[remotePeer]
	s.clientsMutex.RUnlock()

	if !exists {
		if err := s.handleAuthenticationStream(stream); err != nil {
			slog.Warn("Authentication failed", "client", remotePeer.String(), "error", err)
			time.Sleep(500 * time.Millisecond)
			s.host.Network().ClosePeer(remotePeer)
		}
		return
	}

	// Replace old one, close it first
	s.clientsMutex.Lock()
	old := s.authenticatedClients[remotePeer]
	if old != nil {
		delete(s.authenticatedClients, remotePeer)
	}
	s.clientsMutex.Unlock()
	if old != nil {
		if old.controlDispatcher != nil {
			old.controlDispatcher.Close()
		}
		if old.controlStream != nil {
			_ = old.controlStream.Close()
		}
	}

	mess := message.NewMessager(stream, s.config, remotePeer)
	disp := message.NewDispatcher(mess)
	disp.Start()
	cs := &ClientSession{peerID: remotePeer, controlStream: stream, controlMessager: mess, controlDispatcher: disp, authTime: time.Now(), lastSeen: time.Now()}
	s.clientsMutex.Lock()
	s.authenticatedClients[remotePeer] = cs
	s.clientsMutex.Unlock()
	go s.monitorControlStream(cs)
}

func (s *Server) handleAuthenticationStream(stream network.Stream) error {
	remotePeer := stream.Conn().RemotePeer()
	messager := message.NewMessager(stream, s.config, remotePeer)
	disp := message.NewDispatcher(messager)
	disp.Start()
	if err := s.handleAuthOnControl(disp, messager); err != nil {
		disp.Close()
		return fmt.Errorf("handshake failed: %v", err)
	}
	cs := &ClientSession{peerID: remotePeer, controlStream: stream, controlMessager: messager, controlDispatcher: disp, authTime: time.Now(), lastSeen: time.Now()}
	s.clientsMutex.Lock()
	s.authenticatedClients[remotePeer] = cs
	s.clientsMutex.Unlock()
	go s.monitorControlStream(cs)
	return nil
}

func (s *Server) monitorControlStream(clientSession *ClientSession) {
	defer func() {
		s.clientsMutex.Lock()
		// Pretend removing the new session
		current := s.authenticatedClients[clientSession.peerID]
		if current != nil && current.controlStream == clientSession.controlStream {
			delete(s.authenticatedClients, clientSession.peerID)
		}
		s.clientsMutex.Unlock()

		slog.Info("Client disconnected", "client", clientSession.peerID.String())

		if clientSession.controlStream != nil {
			_ = clientSession.controlStream.Close()
		}
		if clientSession.controlDispatcher != nil {
			clientSession.controlDispatcher.Close()
		}
		s.host.Network().ClosePeer(clientSession.peerID)
	}()

	messager := clientSession.controlMessager

	// Shorter heartbeat interval for faster dead peer detection
	heartbeatInterval := 10 * time.Second
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	missed := 0
	// Allow up to 2 missed acks => ~20s worst-case extra detection latency
	const maxMissed = 2

	waitingAck := false
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err := messager.SendHeartbeat(); err != nil {
				slog.Error("Failed to send heartbeat (ping)", "error", err)
				return
			}
			waitingAck = true
		default:
			if clientSession.controlDispatcher == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			// Tighter wait timeout to accelerate heartbeat failure detection
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			var msg *pb.UnifiedMessage
			var err error
			if waitingAck {
				msg, err = clientSession.controlDispatcher.WaitFor(ctx, pb.MessageType_HEARTBEAT, pb.MessageType_SERVICE_REQUEST, pb.MessageType_CLIENT_SHUTDOWN)
			} else {
				msg, err = clientSession.controlDispatcher.WaitFor(ctx, pb.MessageType_SERVICE_REQUEST, pb.MessageType_CLIENT_SHUTDOWN)
			}
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					if waitingAck {
						missed++
						slog.Warn("Heartbeat ack timeout", "client", clientSession.peerID.ShortString(), "missed", missed)
						waitingAck = false
						if missed >= maxMissed {
							slog.Error("Too many missed heartbeat acks, closing session", "client", clientSession.peerID.ShortString())
							return
						}
					}
					clientSession.mutex.Lock()
					clientSession.lastSeen = time.Now()
					clientSession.mutex.Unlock()
					continue
				}
				if err != io.EOF && !strings.Contains(err.Error(), "stream reset") {
					slog.Error("Control stream read error", "error", err)
				}
				return
			}

			if msg.IsShutdownMessage() {
				slog.Info("Received shutdown notification from client", "client", clientSession.peerID.String())
				return
			}

			switch msg.Type {
			case pb.MessageType_HEARTBEAT:
				if waitingAck && msg.Message == "pong" {
					missed = 0
					waitingAck = false
				}
			case pb.MessageType_SERVICE_REQUEST:
				s.handleServiceVerificationRequest(messager, msg)
			default:
				slog.Debug("Received control message from client", "type", msg.Type, "message", msg.Message)
			}

			clientSession.mutex.Lock()
			clientSession.lastSeen = time.Now()
			clientSession.mutex.Unlock()
		}
	}
}

func (s *Server) handleServiceVerificationRequest(handshake *message.Messager, msg *pb.UnifiedMessage) {
	clientID := handshake.PeerID().String()
	slog.Info("Service verification request received", "client", clientID, "service", msg.ServiceName, "protocol", msg.Protocol)

	if msg.ServiceName == "" || msg.Protocol == "" {
		slog.Warn("Invalid service verification request format", "client", clientID)
		_ = handshake.SendServiceResponse(false, pb.ErrorCode_NO_ERROR, "Invalid service request format", nil)
		return
	}

	service, ok := s.services[msg.ServiceName]
	if !ok {
		slog.Warn("Service verification failed: service not found", "client", clientID, "service", msg.ServiceName)
		_ = handshake.SendServiceResponse(false, pb.ErrorCode_SERVICE_NOT_FOUND, fmt.Sprintf("Service '%s' not found on server", msg.ServiceName), nil)
		return
	}
	if !service.Enabled {
		slog.Warn("Service verification failed: service disabled", "client", clientID, "service", msg.ServiceName)
		_ = handshake.SendServiceResponse(false, pb.ErrorCode_SERVICE_DISABLED, fmt.Sprintf("Service '%s' is disabled", msg.ServiceName), nil)
		return
	}
	protocolSet := s.serviceProtocols[msg.ServiceName]
	if _, ok := protocolSet[msg.Protocol]; !ok {
		slog.Warn("Service verification failed: protocol not supported", "client", clientID, "service", msg.ServiceName, "protocol", msg.Protocol)
		_ = handshake.SendServiceResponse(false, pb.ErrorCode_PROTOCOL_NOT_SUPPORTED, fmt.Sprintf("Protocol '%s' not supported for service '%s'. Supported protocols: %v", msg.Protocol, msg.ServiceName, service.Protocol), nil)
		return
	}
	if service.Password != "" {
		if msg.ServicePassword == "" {
			slog.Warn("Service verification failed: password required", "client", clientID, "service", msg.ServiceName)
			_ = handshake.SendServiceResponse(false, pb.ErrorCode_PASSWORD_REQUIRED_ERROR, fmt.Sprintf("Password required for service '%s'", msg.ServiceName), nil)
			return
		}
		if msg.ServicePassword != service.Password {
			slog.Warn("Service verification failed: invalid password", "client", clientID, "service", msg.ServiceName)
			_ = handshake.SendServiceResponse(false, pb.ErrorCode_INVALID_PASSWORD, fmt.Sprintf("Invalid password for service '%s'", msg.ServiceName), nil)
			return
		}
	}
	willCompress := s.config.Server.Compress
	if service.Compress != nil {
		willCompress = *service.Compress
	}
	data := map[string]string{"zstd": fmt.Sprintf("%v", willCompress)}
	_ = handshake.SendServiceResponse(true, pb.ErrorCode_NO_ERROR, fmt.Sprintf("Service '%s' verification successful with protocol '%s'", msg.ServiceName, msg.Protocol), data)
}

// handleServiceHandshake processes a SERVICE_REQUEST from client on a data stream.
// handleServiceHandshake 仅用于数据流上的一次性 SERVICE_REQUEST 握手，不再依赖 dispatcher。
func (s *Server) handleServiceHandshake(messager *message.Messager, requestedService *string, requestedProtocol *string) (*config.ServiceConfig, error) {
	msg, err := messager.ReceiveOne(30 * time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to receive service request: %v", err)
	}
	if msg.Type != pb.MessageType_SERVICE_REQUEST {
		return nil, fmt.Errorf("expected service_request, got %s", msg.Type)
	}
	if msg.ServiceName == "" || msg.Protocol == "" {
		return nil, fmt.Errorf("invalid service request format")
	}
	if requestedService != nil {
		*requestedService = msg.ServiceName
	}
	if requestedProtocol != nil {
		*requestedProtocol = msg.Protocol
	}

	service, ok := s.services[msg.ServiceName]
	if !ok {
		_ = messager.SendServiceResponse(false, pb.ErrorCode_SERVICE_NOT_FOUND, fmt.Sprintf("Service '%s' not found on server", msg.ServiceName), nil)
		return nil, fmt.Errorf("service '%s' not found on server", msg.ServiceName)
	}
	if !service.Enabled {
		_ = messager.SendServiceResponse(false, pb.ErrorCode_SERVICE_DISABLED, fmt.Sprintf("Service '%s' is disabled", msg.ServiceName), nil)
		return nil, fmt.Errorf("service '%s' is disabled", msg.ServiceName)
	}
	protocolSet := s.serviceProtocols[msg.ServiceName]
	if _, ok := protocolSet[msg.Protocol]; !ok {
		_ = messager.SendServiceResponse(false, pb.ErrorCode_PROTOCOL_NOT_SUPPORTED, fmt.Sprintf("Protocol '%s' not supported for service '%s'. Supported protocols: %v", msg.Protocol, msg.ServiceName, service.Protocol), nil)
		return nil, fmt.Errorf("protocol '%s' not supported for service '%s'", msg.Protocol, msg.ServiceName)
	}
	if service.Password != "" {
		if msg.ServicePassword == "" {
			_ = messager.SendServiceResponse(false, pb.ErrorCode_PASSWORD_REQUIRED_ERROR, fmt.Sprintf("Password required for service '%s'", msg.ServiceName), nil)
			return nil, fmt.Errorf("password required for service '%s'", msg.ServiceName)
		}
		if msg.ServicePassword != service.Password {
			_ = messager.SendServiceResponse(false, pb.ErrorCode_INVALID_PASSWORD, fmt.Sprintf("Invalid password for service '%s'", msg.ServiceName), nil)
			return nil, fmt.Errorf("invalid password for service '%s'", msg.ServiceName)
		}
	}
	willCompress := s.config.Server.Compress
	if service.Compress != nil {
		willCompress = *service.Compress
	}
	data := map[string]string{"zstd": fmt.Sprintf("%v", willCompress)}
	if err := messager.SendServiceResponse(true, pb.ErrorCode_NO_ERROR, fmt.Sprintf("Service '%s' ready with protocol '%s'", msg.ServiceName, msg.Protocol), data); err != nil {
		return nil, fmt.Errorf("failed to send service response: %v", err)
	}
	return service, nil
}

// handleAuthOnControl performs server-side password auth on control stream.
func (s *Server) handleAuthOnControl(disp *message.Dispatcher, messager *message.Messager) error {
	passwordRequired := s.config.Server.PasswordHash != ""
	if err := messager.SendPasswordRequired(passwordRequired); err != nil {
		return fmt.Errorf("failed to send password requirement: %v", err)
	}
	const authTimeout = 30 * time.Second
	if passwordRequired {
		slog.Info("Waiting for client authentication", "timeout", "30s", "client", messager.PeerID().String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	defer cancel()
	msg, err := disp.WaitFor(ctx, pb.MessageType_AUTH_REQUEST, pb.MessageType_CLIENT_SHUTDOWN)
	if err != nil {
		_ = messager.SendAuthResponse(false, "timeout")
		return fmt.Errorf("failed to receive auth request: %v", err)
	}
	if msg.Type == pb.MessageType_CLIENT_SHUTDOWN {
		return fmt.Errorf("client requested shutdown during auth")
	}
	if msg.Type != pb.MessageType_AUTH_REQUEST {
		return fmt.Errorf("expected auth_request, got %s", msg.Type)
	}
	ok := (!passwordRequired || config.VerifyPassword(msg.Password, s.config.Server.PasswordHash))
	if !ok {
		_ = messager.SendAuthResponse(false, "Invalid password")
		slog.Warn("Authentication failed: invalid password", "client", messager.PeerID().String())
		return fmt.Errorf("authentication failed: invalid password")
	}
	if err := messager.SendAuthResponse(true, ""); err != nil {
		return fmt.Errorf("failed to send auth response: %v", err)
	}
	slog.Info("Client authentication successful", "client", messager.PeerID().String())
	return nil
}
