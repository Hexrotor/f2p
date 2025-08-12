package server

import (
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
			_ = stream.Reset()
			return
		}
		cs := &ClientSession{peerID: remotePeer, controlStream: stream, authTime: time.Now(), lastSeen: time.Now()}
		s.clientsMutex.Lock()
		s.authenticatedClients[remotePeer] = cs
		s.clientsMutex.Unlock()
		go s.monitorControlStream(cs)
		return
	}

	s.monitorControlStream(&ClientSession{peerID: remotePeer, controlStream: stream, authTime: time.Now(), lastSeen: time.Now()})
}

func (s *Server) handleAuthenticationStream(stream network.Stream) error {
	remotePeer := stream.Conn().RemotePeer()
	handshake := message.NewMessager(stream, s.config, remotePeer)

	_ = stream.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer stream.SetReadDeadline(time.Time{})

	if err := s.handleAuthOnControl(handshake); err != nil {
		return fmt.Errorf("handshake failed: %v", err)
	}
	return nil
}

func (s *Server) monitorControlStream(clientSession *ClientSession) {
	defer func() {
		s.clientsMutex.Lock()
		delete(s.authenticatedClients, clientSession.peerID)
		s.clientsMutex.Unlock()

		slog.Info("Client disconnected", "client", clientSession.peerID.String())

		if clientSession.controlStream != nil {
			_ = clientSession.controlStream.Close()
		}
	}()

	handshake := message.NewMessager(clientSession.controlStream, s.config, clientSession.peerID)

	heartbeatTicker := time.NewTicker(60 * time.Second)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err := handshake.SendHeartbeat(); err != nil {
				slog.Error("Failed to send heartbeat to client", "error", err)
				return
			}
		default:
			msg, err := handshake.ReceiveMessage()
			if err != nil {
				if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "timeout") {
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
				clientID := clientSession.peerID.String()
				suffix := clientID[len(clientID)-8:]
				slog.Info("Received shutdown notification from client", "client", fmt.Sprintf("...%s", suffix))
				return
			}

			switch msg.Type {
			case pb.MessageType_HEARTBEAT:
				// ignore
			case pb.MessageType_SERVICE_REQUEST:
				s.handleServiceVerificationRequest(handshake, msg)
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
	suffix := clientID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	slog.Info("Service verification request received", "client", fmt.Sprintf("...%s", suffix), "service", msg.ServiceName, "protocol", msg.Protocol)

	if msg.ServiceName == "" || msg.Protocol == "" {
		slog.Warn("Invalid service verification request format", "client", fmt.Sprintf("...%s", suffix))
		_ = handshake.SendServiceResponse(false, pb.ErrorCode_NO_ERROR, "Invalid service request format", nil)
		return
	}

	var targetService *config.ServiceConfig
	var serviceExists, protocolSupported bool
	for i := range s.config.Server.Services {
		service := &s.config.Server.Services[i]
		if service.Name == msg.ServiceName {
			serviceExists = true
			if !service.Enabled {
				slog.Warn("Service verification failed: service disabled", "client", fmt.Sprintf("...%s", suffix), "service", msg.ServiceName)
				_ = handshake.SendServiceResponse(false, pb.ErrorCode_SERVICE_DISABLED, fmt.Sprintf("Service '%s' is disabled", msg.ServiceName), nil)
				return
			}
			for _, p := range service.Protocol {
				if p == msg.Protocol {
					protocolSupported = true
					targetService = service
					break
				}
			}
			break
		}
	}

	if !serviceExists {
		slog.Warn("Service verification failed: service not found", "client", fmt.Sprintf("...%s", suffix), "service", msg.ServiceName)
		_ = handshake.SendServiceResponse(false, pb.ErrorCode_SERVICE_NOT_FOUND, fmt.Sprintf("Service '%s' not found on server", msg.ServiceName), nil)
		return
	}

	if !protocolSupported {
		var supported []string
		for _, s := range s.config.Server.Services {
			if s.Name == msg.ServiceName {
				supported = s.Protocol
				break
			}
		}
		slog.Warn("Service verification failed: protocol not supported", "client", fmt.Sprintf("...%s", suffix), "service", msg.ServiceName, "protocol", msg.Protocol, "supported", supported)
		_ = handshake.SendServiceResponse(false, pb.ErrorCode_PROTOCOL_NOT_SUPPORTED, fmt.Sprintf("Protocol '%s' not supported for service '%s'. Supported protocols: %v", msg.Protocol, msg.ServiceName, supported), nil)
		return
	}

	if targetService != nil && targetService.Password != "" {
		if msg.ServicePassword == "" {
			slog.Warn("Service verification failed: password required", "client", fmt.Sprintf("...%s", suffix), "service", msg.ServiceName)
			_ = handshake.SendServiceResponse(false, pb.ErrorCode_PASSWORD_REQUIRED_ERROR, fmt.Sprintf("Password required for service '%s'", msg.ServiceName), nil)
			return
		}
		if msg.ServicePassword != targetService.Password {
			slog.Warn("Service verification failed: invalid password", "client", fmt.Sprintf("...%s", suffix), "service", msg.ServiceName)
			_ = handshake.SendServiceResponse(false, pb.ErrorCode_INVALID_PASSWORD, fmt.Sprintf("Invalid password for service '%s'", msg.ServiceName), nil)
			return
		}
	}

	willCompress := s.config.Server.Compress
	if targetService != nil && targetService.Compress != nil {
		willCompress = *targetService.Compress
	}
	data := map[string]string{"zstd": fmt.Sprintf("%v", willCompress)}
	_ = handshake.SendServiceResponse(true, pb.ErrorCode_NO_ERROR, fmt.Sprintf("Service '%s' verification successful with protocol '%s'", msg.ServiceName, msg.Protocol), data)
}

// handleServiceHandshake processes a SERVICE_REQUEST on a data stream.
func (s *Server) handleServiceHandshake(messager *message.Messager, requestedService *string, requestedProtocol *string) (*config.ServiceConfig, error) {
	msg, err := messager.ReceiveMessage()
	if err != nil {
		return nil, fmt.Errorf("failed to receive message: %v", err)
	}
	if msg.Type == pb.MessageType_CLIENT_SHUTDOWN {
		return nil, fmt.Errorf("client_shutdown received")
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

	var targetService *config.ServiceConfig
	var serviceExists = false
	var protocolSupported = false
	for i := range s.config.Server.Services {
		svc := &s.config.Server.Services[i]
		if svc.Name == msg.ServiceName {
			serviceExists = true
			if !svc.Enabled {
				_ = messager.SendServiceResponse(false, pb.ErrorCode_SERVICE_DISABLED, fmt.Sprintf("Service '%s' is disabled", msg.ServiceName), nil)
				return nil, fmt.Errorf("service '%s' is disabled", msg.ServiceName)
			}
			for _, supportedProtocol := range svc.Protocol {
				if supportedProtocol == msg.Protocol {
					protocolSupported = true
					targetService = svc
					break
				}
			}
			break
		}
	}
	if !serviceExists {
		_ = messager.SendServiceResponse(false, pb.ErrorCode_SERVICE_NOT_FOUND, fmt.Sprintf("Service '%s' not found on server", msg.ServiceName), nil)
		return nil, fmt.Errorf("service '%s' not found on server", msg.ServiceName)
	}
	if !protocolSupported {
		var supportedProtocols []string
		for _, service := range s.config.Server.Services {
			if service.Name == msg.ServiceName {
				supportedProtocols = service.Protocol
				break
			}
		}
		_ = messager.SendServiceResponse(false, pb.ErrorCode_PROTOCOL_NOT_SUPPORTED, fmt.Sprintf("Protocol '%s' not supported for service '%s'. Supported protocols: %v", msg.Protocol, msg.ServiceName, supportedProtocols), nil)
		return nil, fmt.Errorf("protocol '%s' not supported for service '%s'. Supported: %v", msg.Protocol, msg.ServiceName, supportedProtocols)
	}
	if targetService.Password != "" {
		if msg.ServicePassword == "" {
			_ = messager.SendServiceResponse(false, pb.ErrorCode_PASSWORD_REQUIRED_ERROR, fmt.Sprintf("Password required for service '%s'", msg.ServiceName), nil)
			return nil, fmt.Errorf("password required for service '%s'", msg.ServiceName)
		}
		if msg.ServicePassword != targetService.Password {
			_ = messager.SendServiceResponse(false, pb.ErrorCode_INVALID_PASSWORD, fmt.Sprintf("Invalid password for service '%s'", msg.ServiceName), nil)
			return nil, fmt.Errorf("invalid password for service '%s'", msg.ServiceName)
		}
	}
	willCompress := s.config.Server.Compress
	if targetService != nil && targetService.Compress != nil {
		willCompress = *targetService.Compress
	}
	data := map[string]string{"zstd": fmt.Sprintf("%v", willCompress)}
	if err := messager.SendServiceResponse(true, pb.ErrorCode_NO_ERROR, fmt.Sprintf("Service '%s' ready with protocol '%s'", msg.ServiceName, msg.Protocol), data); err != nil {
		return nil, fmt.Errorf("failed to send service response: %v", err)
	}
	return targetService, nil
}

// handleAuthOnControl performs server-side password auth on control stream.
func (s *Server) handleAuthOnControl(messager *message.Messager) error {
	passwordRequired := s.config.Server.PasswordHash != ""
	if err := messager.SendPasswordRequired(passwordRequired); err != nil {
		return fmt.Errorf("failed to send password requirement: %v", err)
	}
	const authTimeout = 30 * time.Second
	if passwordRequired {
		slog.Info("Waiting for client authentication", "timeout", "30s", "client", messager.PeerID().String())
	}
	msg, err := messager.ReceiveMessageWithTimeout(authTimeout)
	if err != nil {
		_ = messager.SendAuthResponse(false, "timeout")
		return fmt.Errorf("failed to receive auth request: %v", err)
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
