package client

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/Hexrotor/f2p/internal/message"
	"github.com/Hexrotor/f2p/internal/utils"
	pb "github.com/Hexrotor/f2p/proto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func (c *Client) verifyServiceConfiguration() error {
	slog.Info("Verifying service configuration")

	c.streamMutex.RLock()
	controlStream := c.controlStream
	c.streamMutex.RUnlock()

	if controlStream == nil || c.serverPeerID == "" {
		return fmt.Errorf("no active control stream available for verification")
	}

	for _, service := range c.config.Client.Services {
		if !service.Enabled {
			slog.Debug("Skipping disabled service", "service", service.Name)
			continue
		}

		for _, protocolType := range service.Protocol {
			slog.Debug("Verifying service", "name", service.Name, "protocol", protocolType)
			if err := c.verifyServiceOnControlStream(service.Name, protocolType, service.Password, controlStream, c.serverPeerID); err != nil {
				slog.Error("Service verification failed", "service", service.Name, "protocol", protocolType, "error", err)
				fmt.Printf("Service verification failed for '%s' (%s): %v\n", service.Name, protocolType, err)
				fmt.Println("Client will terminate due to service verification failure")
				return err
			}
			slog.Info("Service verified successfully", "service", service.Name, "protocol", protocolType)
		}
	}

	slog.Info("All services verified successfully")
	return nil
}

func (c *Client) verifyServiceOnControlStream(serviceName, protocolType, servicePassword string, controlStream network.Stream, serverPeerID peer.ID) error {
	resp, err := c.checkServiceAvailability(controlStream, serverPeerID, serviceName, protocolType, servicePassword)
	if err != nil {
		return err
	}
	if resp.Data != nil {
		if v, ok := resp.Data["zstd"]; ok {
			use := strings.EqualFold(v, "true")
			c.compMutex.Lock()
			c.serviceCompress[serviceName] = use
			c.compMutex.Unlock()
			slog.Debug("Cached service compression decision", "service", serviceName, "zstd", use)
		}
	}
	return nil
}

// checkServiceAvailability sends a SERVICE_REQUEST on the given control stream and returns the response.
func (c *Client) checkServiceAvailability(rw io.ReadWriteCloser, serverPeerID peer.ID, serviceName, protocolType, servicePassword string) (*pb.UnifiedMessage, error) {
	h := message.NewMessager(rw, c.config, serverPeerID)
	if err := h.SendServiceRequest(serviceName, protocolType, servicePassword); err != nil {
		return nil, fmt.Errorf("service verification failed: %v", err)
	}
	resp, err := h.ReceiveMessage()
	if err != nil {
		return nil, fmt.Errorf("service verification failed: %v", err)
	}
	if resp.Type != pb.MessageType_SERVICE_RESPONSE {
		return nil, fmt.Errorf("service verification failed: unexpected message type %s", resp.Type)
	}
	if !resp.Success {
		var detailed string
		switch resp.ErrorCode {
		case pb.ErrorCode_SERVICE_NOT_FOUND:
			detailed = fmt.Sprintf("Service '%s' not found on server. Please check your configuration.", serviceName)
		case pb.ErrorCode_PROTOCOL_NOT_SUPPORTED:
			detailed = fmt.Sprintf("Protocol '%s' not supported for service '%s'. %s", protocolType, serviceName, resp.Message)
		case pb.ErrorCode_SERVICE_DISABLED:
			detailed = fmt.Sprintf("Service '%s' is disabled on server. Please contact server administrator.", serviceName)
		case pb.ErrorCode_PASSWORD_REQUIRED_ERROR:
			detailed = fmt.Sprintf("Password required for service '%s'. Please set service password in client configuration.", serviceName)
		case pb.ErrorCode_INVALID_PASSWORD:
			detailed = fmt.Sprintf("Invalid password for service '%s'. Please check service password in configuration.", serviceName)
		default:
			detailed = resp.Message
		}
		if resp.ErrorCode != pb.ErrorCode_NO_ERROR {
			return nil, fmt.Errorf("[%s] %s", resp.ErrorCode, detailed)
		}
		return nil, fmt.Errorf("service verification failed: %s", detailed)
	}
	return resp, nil
}

func (c *Client) authenticateWithServer(controlStream network.Stream, serverPeerID peer.ID) error {
	messager := message.NewMessager(controlStream, c.config, serverPeerID)
	// Step 1: read PASSWORD_REQUIRED from server
	msg, err := messager.ReceiveMessage()
	if err != nil {
		return fmt.Errorf("failed to receive password requirement: %v", err)
	}
	if msg.Type != pb.MessageType_PASSWORD_REQUIRED {
		return fmt.Errorf("expected password_required, got %s", msg.Type)
	}
	// Step 2: determine password using cache; only prompt when needed
	var password string
	if msg.RequirePassword {
		// Prefer cached password if present
		c.authMu.RLock()
		has := c.hasCachedServerPassword
		cached := c.cachedServerPassword
		c.authMu.RUnlock()
		if has {
			password = cached
		} else {
			password = utils.AskPassword("Enter server password: ")
			// Cache it for future reconnects
			c.authMu.Lock()
			c.cachedServerPassword = password
			c.hasCachedServerPassword = true
			c.authMu.Unlock()
		}
	}
	// Step 3: send auth request
	if err := messager.SendAuthRequest(password); err != nil {
		return fmt.Errorf("failed to send auth request: %v", err)
	}
	// Step 4: read auth response and validate
	resp, err := messager.ReceiveMessage()
	if err != nil {
		return fmt.Errorf("failed to receive auth response: %v", err)
	}
	if resp.Type != pb.MessageType_AUTH_RESPONSE {
		return fmt.Errorf("expected auth_response, got %s", resp.Type)
	}
	if !resp.Success {
		// Clear cached password on failure so next attempt will prompt again
		c.authMu.Lock()
		c.cachedServerPassword = ""
		c.hasCachedServerPassword = false
		c.authMu.Unlock()
		return fmt.Errorf("authentication failed: %s", resp.Message)
	}
	// Auth success: ensure cache remains (useful if we prompted this time)
	if msg.RequirePassword {
		c.authMu.Lock()
		if !c.hasCachedServerPassword {
			c.cachedServerPassword = password
			c.hasCachedServerPassword = true
		}
		c.authMu.Unlock()
	}
	return nil
}
