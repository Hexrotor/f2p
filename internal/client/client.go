package client

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hexrotor/f2p/internal/config"
	message "github.com/Hexrotor/f2p/internal/message"
	"github.com/Hexrotor/f2p/internal/utils"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	basichost "github.com/libp2p/go-libp2p/p2p/host/basic"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
	noise "github.com/libp2p/go-libp2p/p2p/security/noise"
	tls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/multiformats/go-multiaddr"
)

type Client struct {
	config            *config.Config
	host              host.Host
	dht               *dht.IpfsDHT
	ctx               context.Context
	cancel            context.CancelFunc
	localServices     map[string]*config.LocalServiceConfig
	serverPeerID      peer.ID
	controlStream     network.Stream
	controlMessager   *message.Messager
	controlDispatcher *message.Dispatcher
	streamMutex       sync.RWMutex

	compMutex       sync.RWMutex
	serviceCompress map[string]bool

	startServicesOnce sync.Once

	backoffMu  sync.Mutex
	backoff    time.Duration
	backoffMax time.Duration

	// cached server auth password (memory only). Use hasCachedServerPassword to
	// distinguish between "not set" and an intentionally empty string.
	authMu                  sync.RWMutex
	cachedServerPassword    string
	hasCachedServerPassword bool

	// unified shutdown reason
	shutdownMu     sync.Mutex
	shutdownReason string
	stopping       atomic.Bool
}

func NewClient(cfg *config.Config) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	localServices := make(map[string]*config.LocalServiceConfig)
	for i := range cfg.Client.Services {
		service := &cfg.Client.Services[i]
		localServices[service.Name] = service
	}

	return &Client{
		config:          cfg,
		ctx:             ctx,
		cancel:          cancel,
		localServices:   localServices,
		serviceCompress: make(map[string]bool),
		backoff:         1 * time.Second,
		backoffMax:      60 * time.Second,
	}
}

func (c *Client) Start() error {
	fmt.Println("Starting P2P Forward Client...")

	utils.ConfigureZstdParams(
		c.config.Common.ZstdLevel,
		c.config.Common.ZstdMinSizeB,
		c.config.Common.ZstdChunkSizeKB*1024,
	)

	var err error
	c.serverPeerID, err = peer.Decode(c.config.Client.ServerID)
	if err != nil {
		return fmt.Errorf("failed to decode server peer ID: %v", err)
	}

	privKey, err := c.generateClientKey()
	if err != nil {
		return fmt.Errorf("failed to generate client key: %v", err)
	}

	if c.config.Identity.PrivateKey == "" {
		privKeyBytes, err := crypto.MarshalPrivateKey(privKey)
		if err != nil {
			slog.Warn("Failed to marshal private key", "error", err)
		} else {
			c.config.Identity.PrivateKey = base64.StdEncoding.EncodeToString(privKeyBytes)

			peerID, err := peer.IDFromPrivateKey(privKey)
			if err != nil {
				slog.Warn("Failed to generate peer ID", "error", err)
			} else {
				c.config.Identity.PeerID = peerID.String()
			}
		}
	}

	var listenAddrs []multiaddr.Multiaddr
	for _, addrStr := range c.config.Common.Listen {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			slog.Warn("Invalid listen address", "address", addrStr, "error", err)
			continue
		}
		listenAddrs = append(listenAddrs, addr)
	}

	// Build hole punch options: always enable v2 (includes default STUN),
	// optionally override STUN servers with user-configured ones.
	hpOpts := []holepunch.Option{holepunch.EnableV2()}
	if len(c.config.Common.StunServers) >= 2 {
		hpOpts = append(hpOpts, holepunch.WithSTUNServers(c.config.Common.StunServers))
	}

	opts := []libp2p.Option{
		libp2p.Identity(privKey),
		libp2p.ListenAddrs(listenAddrs...),
		libp2p.EnableHolePunching(hpOpts...),
		libp2p.EnableAutoNATv2(),
		libp2p.EnableNATService(),
		libp2p.UserAgent(utils.GetUserAgent()),
		libp2p.NATPortMap(),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Security(tls.ID, tls.New),
		libp2p.SwarmOpts(swarm.WithDialTimeout(5 * time.Second)),
	}

	c.host, err = libp2p.New(opts...)
	if err != nil {
		return fmt.Errorf("failed to create libp2p host: %v", err)
	}

	slog.Info("Client created", "id", c.host.ID())

	c.logDCUtRStatus()

	bootstrapPeers := dht.GetDefaultBootstrapPeerAddrInfos()

	c.dht, err = dht.New(c.ctx, c.host, dht.BootstrapPeers(bootstrapPeers...))
	if err != nil {
		return fmt.Errorf("failed to create DHT: %v", err)
	}

	slog.Info("Bootstrapping DHT")
	if err = c.dht.Bootstrap(c.ctx); err != nil {
		return fmt.Errorf("failed to bootstrap DHT: %v", err)
	}
	c.dht.RefreshRoutingTable()

	for {
		time.Sleep(time.Second * 5)
		if len(c.dht.RoutingTable().ListPeers()) > 0 {
			break
		}
	}

	go c.startConnectionManager()
	return nil
}

func (c *Client) startConnectionManager() {
	slog.Info("Starting connection manager")

	for {
		select {
		case <-c.ctx.Done():
			c.shutdownMu.Lock()
			if c.shutdownReason == "" {
				c.shutdownReason = "context cancelled"
			}
			c.shutdownMu.Unlock()
			return
		default:
			if err := c.connectToServer(c.serverPeerID); err != nil {
				select {
				case <-c.ctx.Done():
					return
				default:
				}

				slog.Error("Connection failed", "error", err, "server", c.serverPeerID.ShortString())
				interval := c.GetBackoff()
				fmt.Printf("Retrying in %v...\n", interval)

				select {
				case <-c.ctx.Done():
					return
				case <-time.After(interval):
					c.IncreaseBackoff()
					continue
				}
			}
			c.ResetBackoff()
			return
		}
	}
}

func (c *Client) Stop() error {
	c.shutdownMu.Lock()
	reason := c.shutdownReason
	c.shutdownMu.Unlock()

	if reason == "" {
		reason = "user requested stop"
	}
	fmt.Println()
	slog.Info("Stopping client", "reason", reason)

	c.streamMutex.RLock()
	controlStream := c.controlStream
	c.streamMutex.RUnlock()

	c.stopping.Store(true)

	if controlStream != nil {
		if c.controlMessager != nil {
			_ = c.controlMessager.SendClientShutdownNotification()
			// small delay to allow message flush across multiplex/encryption layers
			time.Sleep(1 * time.Second)
		}
		controlStream.Close()
	}
	c.streamMutex.Lock()
	if c.controlDispatcher != nil {
		c.controlDispatcher.Close()
	}
	c.streamMutex.Unlock()

	c.cancel()

	if c.dht != nil {
		c.dht.Close()
	}

	if c.host != nil {
		return c.host.Close()
	}

	return nil
}

func (c *Client) ResetBackoff() {
	c.backoffMu.Lock()
	c.backoff = 1 * time.Second
	c.backoffMu.Unlock()
}

func (c *Client) GetBackoff() time.Duration {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	return c.backoff
}

func (c *Client) IncreaseBackoff() {
	c.backoffMu.Lock()
	next := time.Duration(float64(c.backoff) * 1.5)
	if next > c.backoffMax {
		next = c.backoffMax
	}
	c.backoff = next
	c.backoffMu.Unlock()
}

func (c *Client) Wait() {
	<-c.ctx.Done()
}

// setShutdownReason sets reason only once (first wins)
func (c *Client) setShutdownReason(r string) {
	c.shutdownMu.Lock()
	if c.shutdownReason == "" {
		c.shutdownReason = r
	}
	c.shutdownMu.Unlock()
}

// SetShutdownReason allows external packages (main) to assign a user-friendly exit reason.
func (c *Client) SetShutdownReason(r string) { c.setShutdownReason(r) }

func (c *Client) generateClientKey() (crypto.PrivKey, error) {
	if c.config.Identity.PrivateKey != "" {
		privKey, err := config.LoadPrivateKeyFromB64(c.config.Identity.PrivateKey)
		if err == nil {
			return privKey, nil
		}
		slog.Warn("Failed to load saved private key, generating new one", "error", err)
	}

	privKey, _, err := crypto.GenerateEd25519Key(nil)
	return privKey, err
}

func (c *Client) printClientInfo() {
	var output strings.Builder

	output.WriteString("\n" + strings.Repeat("=", 60) + "\n")
	output.WriteString("F2P Client Ready!\n")
	output.WriteString(strings.Repeat("=", 60) + "\n")

	output.WriteString(fmt.Sprintf("Connected to Server: %s\n", c.serverPeerID.ShortString()))

	// Show connection type (direct vs relay)
	connType := "unknown"
	for _, conn := range c.host.Network().ConnsToPeer(c.serverPeerID) {
		addr := conn.RemoteMultiaddr().String()
		if strings.Contains(addr, "p2p-circuit") {
			connType = "relay"
		} else {
			connType = "direct"
			break // prefer direct
		}
	}
	output.WriteString(fmt.Sprintf("Connection Type: %s\n", connType))

	output.WriteString("\nLocal Services:\n")
	for _, service := range c.localServices {
		serviceStatus := "Disabled"
		if service.Enabled {
			serviceStatus = "Enabled"
		}
		output.WriteString(fmt.Sprintf("   * %s - %s\n", service.Name, serviceStatus))

		if service.Enabled {
			protocolsStr := strings.Join(service.Protocol, ", ")
			output.WriteString(fmt.Sprintf("     ├─ Local: %s\n", service.Local))
			output.WriteString(fmt.Sprintf("     └─ Protocols: %s\n", protocolsStr))
		}
	}

	// Show DCUtR v2 status
	if bh, ok := c.host.(*basichost.BasicHost); ok {
		if hps := bh.HolePunchService(); hps != nil {
			natType := hps.NATType()
			output.WriteString(fmt.Sprintf("\nDCUtR v2: enabled (NAT: %s)\n", natType))
		} else {
			output.WriteString("\nDCUtR v2: disabled\n")
		}
	}

	output.WriteString(strings.Repeat("=", 60) + "\n\n")

	fmt.Print(output.String())
}

// logDCUtRStatus starts a background goroutine that waits for NAT detection
// to complete and logs the result. Since NAT detection depends on having
// enough observations from other peers, this may not be available immediately.
func (c *Client) logDCUtRStatus() {
	bh, ok := c.host.(*basichost.BasicHost)
	if !ok {
		return
	}
	hps := bh.HolePunchService()
	if hps == nil {
		return
	}

	go func() {
		// Wait a bit for identify to exchange observations
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				natType := hps.NATType()
				if natType != holepunch.NATUnknown {
					slog.Info("DCUtR v2 NAT detected", "type", natType.String())
					return
				}
			}
		}
	}()
}
