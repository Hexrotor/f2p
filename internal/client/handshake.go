package client

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Hexrotor/f2p/internal/message"
	"github.com/Hexrotor/f2p/internal/utils"
	pb "github.com/Hexrotor/f2p/proto"
)

func (c *Client) verifyServiceConfiguration() error {
	slog.Info("Verifying service configuration")

	for _, service := range c.config.Client.Services {
		if !service.Enabled {
			slog.Debug("Skipping disabled service", "service", service.Name)
			continue
		}

		for _, protocolType := range service.Protocol {
			slog.Debug("Verifying service", "name", service.Name, "protocol", protocolType)
			if err := c.verifyServiceOnControlStream(service.Name, protocolType, service.Password); err != nil {
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

func (c *Client) verifyServiceOnControlStream(serviceName, protocolType, servicePassword string) error {
	c.streamMutex.RLock()
	disp := c.controlDispatcher
	m := c.controlMessager
	c.streamMutex.RUnlock()
	if disp == nil || m == nil {
		return fmt.Errorf("control dispatcher not ready")
	}
	resp, err := c.checkServiceAvailability(m, disp, serviceName, protocolType, servicePassword)
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

// checkServiceAvailability send SERVICE_REQUEST and wait for SERVICE_RESPONSE message
// used for control stream handshake service verification and data stream service handshake
func (c *Client) checkServiceAvailability(m *message.Messager, disp *message.Dispatcher, serviceName, protocolType, servicePassword string) (*pb.UnifiedMessage, error) {
	if m == nil {
		return nil, fmt.Errorf("messager not ready")
	}
	if err := m.SendServiceRequest(serviceName, protocolType, servicePassword); err != nil {
		return nil, fmt.Errorf("service verification failed: %v", err)
	}
	var resp *pb.UnifiedMessage
	var err error
	if disp != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err = disp.WaitFor(ctx, pb.MessageType_SERVICE_RESPONSE)
	} else {
		// 数据流一次性模式：直接同步读取一条
		resp, err = m.ReceiveOne(30 * time.Second)
	}
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

func (c *Client) authenticateWithServer() error {
	c.streamMutex.RLock()
	disp := c.controlDispatcher
	m := c.controlMessager
	c.streamMutex.RUnlock()
	if disp == nil || m == nil {
		return fmt.Errorf("dispatcher not initialized")
	}
	ctxPwd, cancelPwd := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelPwd()
	msg, err := disp.WaitFor(ctxPwd, pb.MessageType_PASSWORD_REQUIRED)
	if err != nil {
		return fmt.Errorf("failed to receive password requirement: %v", err)
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
		}
	}
	// Step 3: send auth request
	if err := m.SendAuthRequest(password); err != nil {
		return fmt.Errorf("failed to send auth request: %v", err)
	}
	ctxAuth, cancelAuth := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelAuth()
	resp, err := disp.WaitFor(ctxAuth, pb.MessageType_AUTH_RESPONSE)
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
