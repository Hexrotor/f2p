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
	slog.Info("New control stream", "client", remotePeer.String(), "addr", stream.Conn().RemoteMultiaddr())

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
	s.host.ConnManager().TagPeer(remotePeer, "authenticated-client", 100)
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
	// 防止 ConnManager 修剪已认证客户端的连接
	s.host.ConnManager().TagPeer(remotePeer, "authenticated-client", 100)
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

		s.host.ConnManager().UntagPeer(clientSession.peerID, "authenticated-client")

		if clientSession.controlStream != nil {
			_ = clientSession.controlStream.Close()
		}
		if clientSession.controlDispatcher != nil {
			clientSession.controlDispatcher.Close()
		}
		s.host.Network().ClosePeer(clientSession.peerID)
	}()

	messager := clientSession.controlMessager
	disp := clientSession.controlDispatcher

	// Feed incoming messages into a channel so the select below is purely channel-driven.
	type msgResult struct {
		msg *pb.UnifiedMessage
		err error
	}
	readerCtx, readerCancel := context.WithCancel(s.ctx)
	defer readerCancel()

	msgCh := make(chan msgResult, 1)
	go func() {
		for {
			// Block until next message; use a long timeout so we don't spin.
			ctx, cancel := context.WithTimeout(readerCtx, 60*time.Second)
			msg, err := disp.WaitFor(ctx,
				pb.MessageType_HEARTBEAT,
				pb.MessageType_SERVICE_REQUEST,
				pb.MessageType_CLIENT_SHUTDOWN,
			)
			cancel()
			if err != nil {
				if ctx.Err() == context.DeadlineExceeded {
					continue // just a timeout, keep reading
				}
				if readerCtx.Err() != nil {
					return // parent cancelled, exit silently
				}
				msgCh <- msgResult{err: err}
				return
			}
			msgCh <- msgResult{msg: msg}
		}
	}()

	heartbeatTicker := time.NewTicker(10 * time.Second)
	defer heartbeatTicker.Stop()

	missed := 0
	const maxMissed = 2
	waitingAck := false

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-heartbeatTicker.C:
			if waitingAck {
				missed++
				slog.Warn("Heartbeat ack timeout", "client", clientSession.peerID.ShortString(), "missed", missed)
				waitingAck = false
				if missed >= maxMissed {
					slog.Warn("Too many missed heartbeat acks, closing session", "client", clientSession.peerID.ShortString())
					return
				}
			}
			if err := messager.SendHeartbeat(); err != nil {
				slog.Error("Failed to send heartbeat (ping)", "error", err)
				return
			}
			waitingAck = true
		case r := <-msgCh:
			if r.err != nil {
				if r.err != io.EOF && !strings.Contains(r.err.Error(), "stream reset") && !strings.Contains(r.err.Error(), "closed") {
					slog.Error("Control stream read error", "error", r.err)
				}
				return
			}
			clientSession.mutex.Lock()
			clientSession.lastSeen = time.Now()
			clientSession.mutex.Unlock()

			if r.msg.IsShutdownMessage() {
				slog.Info("Received shutdown notification from client", "client", clientSession.peerID.String())
				return
			}
			switch r.msg.Type {
			case pb.MessageType_HEARTBEAT:
				if waitingAck && r.msg.Message == "pong" {
					missed = 0
					waitingAck = false
				}
			case pb.MessageType_SERVICE_REQUEST:
				s.handleServiceVerificationRequest(messager, r.msg)
			default:
				slog.Debug("Received control message from client", "type", r.msg.Type, "message", r.msg.Message)
			}
		}
	}
}

// validateServiceRequest checks service existence, enabled status, protocol support, and password.
// Returns the service config and compression metadata on success, or an error code and message on failure.
func (s *Server) validateServiceRequest(msg *pb.UnifiedMessage) (*config.ServiceConfig, map[string]string, pb.ErrorCode, string) {
	if msg.ServiceName == "" || msg.Protocol == "" {
		return nil, nil, pb.ErrorCode_NO_ERROR, "Invalid service request format"
	}
	service, ok := s.services[msg.ServiceName]
	if !ok {
		return nil, nil, pb.ErrorCode_SERVICE_NOT_FOUND, fmt.Sprintf("Service '%s' not found on server", msg.ServiceName)
	}
	if !service.Enabled {
		return nil, nil, pb.ErrorCode_SERVICE_DISABLED, fmt.Sprintf("Service '%s' is disabled", msg.ServiceName)
	}
	protocolSet := s.serviceProtocols[msg.ServiceName]
	if _, ok := protocolSet[msg.Protocol]; !ok {
		return nil, nil, pb.ErrorCode_PROTOCOL_NOT_SUPPORTED, fmt.Sprintf("Protocol '%s' not supported for service '%s'. Supported protocols: %v", msg.Protocol, msg.ServiceName, service.Protocol)
	}
	if service.Password != "" {
		if msg.ServicePassword == "" {
			return nil, nil, pb.ErrorCode_PASSWORD_REQUIRED_ERROR, fmt.Sprintf("Password required for service '%s'", msg.ServiceName)
		}
		if msg.ServicePassword != service.Password {
			return nil, nil, pb.ErrorCode_INVALID_PASSWORD, fmt.Sprintf("Invalid password for service '%s'", msg.ServiceName)
		}
	}
	willCompress := s.config.Server.Compress
	if service.Compress != nil {
		willCompress = *service.Compress
	}
	data := map[string]string{"zstd": fmt.Sprintf("%v", willCompress)}
	return service, data, pb.ErrorCode_NO_ERROR, fmt.Sprintf("Service '%s' ready with protocol '%s'", msg.ServiceName, msg.Protocol)
}

func (s *Server) handleServiceVerificationRequest(handshake *message.Messager, msg *pb.UnifiedMessage) {
	clientID := handshake.PeerID().String()
	slog.Info("Service verification request received", "client", clientID, "service", msg.ServiceName, "protocol", msg.Protocol)

	service, data, errCode, errMsg := s.validateServiceRequest(msg)
	if service == nil {
		slog.Warn("Service verification failed", "client", clientID, "service", msg.ServiceName, "code", errCode)
		_ = handshake.SendServiceResponse(false, errCode, errMsg, nil)
		return
	}
	_ = handshake.SendServiceResponse(true, pb.ErrorCode_NO_ERROR, errMsg, data)
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

	service, data, errCode, errMsg := s.validateServiceRequest(msg)
	if service == nil {
		_ = messager.SendServiceResponse(false, errCode, errMsg, nil)
		return nil, fmt.Errorf("%s", errMsg)
	}
	if err := messager.SendServiceResponse(true, pb.ErrorCode_NO_ERROR, errMsg, data); err != nil {
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
