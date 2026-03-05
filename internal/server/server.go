package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Hexrotor/f2p/internal/compress"
	"github.com/Hexrotor/f2p/internal/config"
	"github.com/Hexrotor/f2p/internal/holepunch"
	"github.com/Hexrotor/f2p/internal/message"
	"github.com/Hexrotor/f2p/internal/version"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"
)

type Server struct {
	config               *config.Config
	host                 host.Host
	dht                  *dht.IpfsDHT
	ctx                  context.Context
	cancel               context.CancelFunc
	services             map[string]*config.ServiceConfig
	serviceProtocols     map[string]map[string]struct{} // per service protocol set for O(1) lookup
	authenticatedClients map[peer.ID]*ClientSession
	clientsMutex         sync.RWMutex

	// Hole punching
	natInfo     *holepunch.NATInfo
	natInfoMu   sync.RWMutex
	natDetected bool
}

type ClientSession struct {
	peerID            peer.ID
	controlStream     network.Stream
	controlMessager   *message.Messager
	controlDispatcher *message.Dispatcher
	authTime          time.Time
	lastSeen          time.Time
	mutex             sync.RWMutex
}

func NewServer(cfg *config.Config) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	services := make(map[string]*config.ServiceConfig)
	serviceProtocols := make(map[string]map[string]struct{})
	for i := range cfg.Server.Services {
		service := &cfg.Server.Services[i]
		services[service.Name] = service
		set := make(map[string]struct{}, len(service.Protocol))
		for _, p := range service.Protocol {
			set[p] = struct{}{}
		}
		serviceProtocols[service.Name] = set
	}

	return &Server{
		config:               cfg,
		ctx:                  ctx,
		cancel:               cancel,
		services:             services,
		serviceProtocols:     serviceProtocols,
		authenticatedClients: make(map[peer.ID]*ClientSession),
	}
}

func (s *Server) generateFixedKey() (crypto.PrivKey, error) {
	if s.config.Identity.PrivateKey != "" {
		privKey, err := config.LoadPrivateKeyFromB64(s.config.Identity.PrivateKey)
		if err == nil {
			return privKey, nil
		}
		slog.Warn("Failed to load saved private key, generating new one", "error", err)
	}

	privKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	return privKey, err
}

func (s *Server) Start() error {
	slog.Info("Starting F2P Server")

	compress.ConfigureZstdParams(
		s.config.Common.ZstdLevel,
		s.config.Common.ZstdMinSizeB,
		s.config.Common.ZstdChunkSizeKB*1024,
	)
	privKey, err := s.generateFixedKey()
	if err != nil {
		return fmt.Errorf("failed to generate fixed key: %v", err)
	}

	var listenAddrs []multiaddr.Multiaddr
	for _, addrStr := range s.config.Common.Listen {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			slog.Warn("Invalid listen address", "address", addrStr, "error", err)
			continue
		}
		listenAddrs = append(listenAddrs, addr)
	}

	opts := []libp2p.Option{
		libp2p.Identity(privKey),
		libp2p.ListenAddrs(listenAddrs...),
		libp2p.EnableRelay(),
		libp2p.EnableAutoNATv2(),
		libp2p.EnableHolePunching(),
		libp2p.EnableNATService(),
		libp2p.NATPortMap(),
		libp2p.EnableAutoRelayWithPeerSource(
			s.createAutoRelayPeerSource(),
			autorelay.WithMaxCandidateAge(time.Minute*30),
			autorelay.WithMinInterval(time.Minute*15),
			autorelay.WithNumRelays(3),
		),
		libp2p.UserAgent(version.UserAgent()),
	}

	// ConnManager: 控制空闲连接数量，超过 HighWater 时修剪最不活跃的连接
	cm, err := connmgr.NewConnManager(
		20, // LowWater: 保持至少 20 个连接
		40, // HighWater: 超过 40 个开始修剪
		connmgr.WithGracePeriod(time.Minute),
	)
	if err != nil {
		slog.Warn("Failed to create connection manager", "error", err)
	} else {
		opts = append(opts, libp2p.ConnectionManager(cm))
		slog.Info("Connection manager enabled", "low", 20, "high", 40)
	}

	// ResourceManager: 根据系统资源自动缩放连接数、流数、内存限制
	limiter := rcmgr.NewFixedLimiter(rcmgr.DefaultLimits.AutoScale())
	rm, err := rcmgr.NewResourceManager(limiter)
	if err != nil {
		slog.Warn("Failed to create resource manager", "error", err)
	} else {
		opts = append(opts, libp2p.ResourceManager(rm))
		slog.Info("Resource manager enabled (auto-scaled limits)")
	}

	s.host, err = libp2p.New(opts...)
	if err != nil {
		return fmt.Errorf("failed to create libp2p host: %v", err)
	}

	// Server's full peer ID may not be logged for security reasons?
	slog.Info("Server created", "id", s.host.ID().ShortString())
	slog.Info("Server listening on", "addresses", s.host.Addrs())

	// We use protocol /control for f2p message communication
	// Use /data for no zstd compression data transfer
	// Use /data+zstd for smart zstd compression transfer
	ctrlProto := protocol.ID(version.ControlProtocol(s.config.Common.Protocol))
	dataProto := protocol.ID(version.DataProtocol(s.config.Common.Protocol))
	dataZstdProto := protocol.ID(version.DataProtocolZstd(s.config.Common.Protocol))

	s.host.SetStreamHandler(ctrlProto, s.handleControlStream)
	s.host.SetStreamHandler(dataProto, s.handleDataStream)
	s.host.SetStreamHandler(dataZstdProto, s.handleDataStream)

	// Register hole punch signaling handler
	s.host.SetStreamHandler(protocol.ID(version.SignalingProtocol(s.config.Common.Protocol)), s.handleHolePunchSignaling)

	bootstrapPeers := dht.GetDefaultBootstrapPeerAddrInfos()

	s.dht, err = dht.New(s.ctx, s.host, dht.BootstrapPeers(bootstrapPeers...))
	if err != nil {
		return fmt.Errorf("failed to create DHT: %v", err)
	}

	fmt.Println("Bootstrapping DHT...")
	if err = s.dht.Bootstrap(s.ctx); err != nil {
		return fmt.Errorf("failed to bootstrap DHT: %v", err)
	}

	for {
		if len(s.dht.RoutingTable().ListPeers()) > 0 {
			break
		}
		time.Sleep(time.Second)
	}

	fmt.Println("Waiting for relay...")
	relayTimeout := time.After(60 * time.Second)
	relayTicker := time.NewTicker(2 * time.Second)
	defer relayTicker.Stop()

	relayFound := false
	for !relayFound {
		select {
		case <-relayTimeout:
			fmt.Println("Timeout waiting for relay circuit addresses, continuing anyway...")
			relayFound = true
		case <-relayTicker.C:
			addrs := s.host.Addrs()
			for _, addr := range addrs {
				addrStr := addr.String()
				if strings.Contains(addrStr, "p2p-circuit") {
					relayFound = true
					break
				}
			}
		case <-s.ctx.Done():
			return fmt.Errorf("context cancelled while waiting for relay")
		}
	}

	s.printServerInfo()

	// Detect NAT type in background after peers are connected
	// (libp2p observed addresses should be available by now)
	go s.detectNATType()

	return nil
}

func (s *Server) createAutoRelayPeerSource() func(ctx context.Context, numPeers int) <-chan peer.AddrInfo {
	return func(ctx context.Context, numPeers int) <-chan peer.AddrInfo {
		slog.Debug("Fetching closest peers for AutoRelay")
		peerChan := make(chan peer.AddrInfo)
		go func() {
			defer close(peerChan)

			closestPeers, err := s.dht.GetClosestPeers(ctx, s.host.ID().String())
			if err != nil {
				slog.Error("Failed to get closest peers", "error", err)
				return
			}

			count := 0
			for _, peerID := range closestPeers {
				if numPeers > 0 && count >= numPeers {
					break
				}

				if peerID == s.host.ID() {
					continue
				}

				addrs := s.host.Peerstore().Addrs(peerID)
				if len(addrs) == 0 {
					continue
				}

				select {
				case <-ctx.Done():
					return
				case peerChan <- peer.AddrInfo{ID: peerID, Addrs: addrs}:
					count++
				}
			}
		}()
		return peerChan
	}
}

func (s *Server) Stop() error {
	fmt.Println("Stopping server...")
	s.notifyClientsServerShutdown()
	if s.dht != nil {
		_ = s.dht.Close()
	}
	if s.host != nil {
		_ = s.host.Close()
	}
	s.cancel()
	return nil
}

func (s *Server) Wait() {
	<-s.ctx.Done()
}
