package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/Hexrotor/f2p/internal/config"
	"github.com/Hexrotor/f2p/internal/utils"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/multiformats/go-multiaddr"
)

type Server struct {
	config               *config.Config
	host                 host.Host
	dht                  *dht.IpfsDHT
	ctx                  context.Context
	cancel               context.CancelFunc
	services             map[string]*config.ServiceConfig
	authenticatedClients map[peer.ID]*ClientSession
	clientsMutex         sync.RWMutex
}

type ClientSession struct {
	peerID        peer.ID
	controlStream network.Stream
	authTime      time.Time
	lastSeen      time.Time
	mutex         sync.RWMutex
}

func NewServer(cfg *config.Config) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	services := make(map[string]*config.ServiceConfig)
	for i := range cfg.Server.Services {
		service := &cfg.Server.Services[i]
		services[service.Name] = service
	}

	return &Server{
		config:               cfg,
		ctx:                  ctx,
		cancel:               cancel,
		services:             services,
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

	privKey, _, err := crypto.GenerateEd25519Key(nil)
	return privKey, err
}

func (s *Server) Start() error {
	slog.Info("Starting F2P Server")

	utils.ConfigureZstdParams(
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
		libp2p.EnableAutoRelayWithPeerSource(s.createAutoRelayPeerSource()),
		libp2p.UserAgent(utils.GetUserAgent()),
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
	ctrlProto := protocol.ID(utils.ControlProtocol(s.config.Common.Protocol))
	dataProto := protocol.ID(utils.DataProtocol(s.config.Common.Protocol))
	dataZstdProto := protocol.ID(utils.DataProtocolZstd(s.config.Common.Protocol))

	s.host.SetStreamHandler(ctrlProto, s.handleControlStream)
	s.host.SetStreamHandler(dataProto, s.handleDataStream)
	s.host.SetStreamHandler(dataZstdProto, s.handleDataStream)

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
