package config

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Hexrotor/f2p/internal/utils"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

type Config struct {
	IsServer bool `toml:"is_server"`

	Identity IdentityConfig `toml:"identity"`

	Server ServerConfig `toml:"server,omitempty"`
	Client ClientConfig `toml:"client,omitempty"`

	Common CommonConfig `toml:"common"`
}

type IdentityConfig struct {
	PrivateKey string `toml:"private_key"`
	PeerID     string `toml:"peer_id"`
}

type ServerConfig struct {
	PasswordHash string `toml:"password_hash"`
	Compress     bool   `toml:"compress"` // General compression behavior, enforced by the server
	// Considering low-CPU client devices, may add client-side switch in the future.
	Services []ServiceConfig `toml:"services"`
}

type ServiceConfig struct {
	Name     string   `toml:"name"`
	Target   string   `toml:"target"`
	Password string   `toml:"password"`
	Protocol []string `toml:"protocol"`
	Enabled  bool     `toml:"enabled"`
	Compress *bool    `toml:"compress,omitempty"` // Service-level compression behavior (override), can be nil
}

type ClientConfig struct {
	ServerID string `toml:"server_id"`

	Services []LocalServiceConfig `toml:"services"`
}

type LocalServiceConfig struct {
	Name     string   `toml:"name"`     // name of remote service exported
	Local    string   `toml:"local"`    // service local destination
	Password string   `toml:"password"` // corresponding service password
	Protocol []string `toml:"protocol"`
	Enabled  bool     `toml:"enabled"`
}

type CommonConfig struct {
	Protocol        string   `toml:"protocol"`
	Listen          []string `toml:"listen"`
	LogLevel        string   `toml:"log_level"`
	ZstdLevel       int      `toml:"zstd_level"`
	ZstdMinSizeB    int      `toml:"zstd_min_size_b"`
	ZstdChunkSizeKB int      `toml:"zstd_chunk_size_kb"`
}

var True = true

func GeneratePeerIdentity() (string, string, error) {
	privKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate private key: %v", err)
	}

	privKeyBytes, err := crypto.MarshalPrivateKey(privKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal private key: %v", err)
	}
	privKeyB64 := base64.StdEncoding.EncodeToString(privKeyBytes)

	peerID, err := peer.IDFromPrivateKey(privKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate peer ID: %v", err)
	}

	return privKeyB64, peerID.String(), nil
}

func LoadPrivateKeyFromB64(privKeyB64 string) (crypto.PrivKey, error) {
	privKeyBytes, err := base64.StdEncoding.DecodeString(privKeyB64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode private key: %v", err)
	}

	privKey, err := crypto.UnmarshalPrivateKey(privKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal private key: %v", err)
	}

	return privKey, nil
}

func Load(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse TOML config: %v", err)
	}

	// Enable zstd compression by default for servers, as P2P bandwidth may be limited.
	// The cgo implementation is used because tests showed the pure Go version consumes excessive memory.
	// Further testing is required to evaluate zstd's CPU overhead - it's unclear if default compression is optimal.
	if cfg.IsServer && !meta.IsDefined("server", "compress") {
		cfg.Server.Compress = true
	}

	// - min size（B）: 32..2048，must be less than chunk size（B）
	// - chunk size（KB）: 8..128
	// - zstd level: 1..20
	const (
		defaultMinB    = 256 // 256B
		defaultChunkKB = 32  // 32KB
		defaultLevel   = 20  // zstd level default BEST, need to test whether 20 is suitable
		minMinB        = 32  // 32B
		maxMinB        = 2048
		minChunkKB     = 8   // 8KB
		maxChunkKB     = 128 // 128KB
	)

	hasMin := meta.IsDefined("common", "zstd_min_size_b")
	hasChunk := meta.IsDefined("common", "zstd_chunk_size_kb")
	hasLevel := meta.IsDefined("common", "zstd_level")

	minB := cfg.Common.ZstdMinSizeB
	chunkKB := cfg.Common.ZstdChunkSizeKB
	level := cfg.Common.ZstdLevel
	if !hasMin {
		minB = defaultMinB
	}
	if !hasChunk {
		chunkKB = defaultChunkKB
	}
	if !hasLevel {
		level = defaultLevel
	}

	if chunkKB < minChunkKB {
		chunkKB = minChunkKB
	} else if chunkKB > maxChunkKB {
		chunkKB = maxChunkKB
	}

	if minB < minMinB {
		minB = minMinB
	} else if minB > maxMinB {
		minB = maxMinB
	}

	// minB must less than chunkB
	chunkB := chunkKB * 1024
	if minB >= chunkB {
		minB = chunkB - 1
		if minB < minMinB {
			minB = minMinB
		}
	}

	cfg.Common.ZstdMinSizeB = minB
	cfg.Common.ZstdChunkSizeKB = chunkKB

	if level < 1 {
		level = 1
	} else if level > 20 {
		level = 20
	}
	cfg.Common.ZstdLevel = level

	return &cfg, nil
}

func Save(cfg *Config, configPath string) error {
	var buffer strings.Builder

	encoder := toml.NewEncoder(&buffer)
	encoder.Indent = ""

	if cfg.IsServer {
		type ServerServiceForTOML struct {
			Name     string   `toml:"name"`
			Target   string   `toml:"target"`
			Password *string  `toml:"password,omitempty"`
			Protocol []string `toml:"protocol"`
			Enabled  bool     `toml:"enabled"`
			Compress *bool    `toml:"compress,omitempty"`
		}

		type ServerConfigForTOML struct {
			PasswordHash string                 `toml:"password_hash,omitempty"`
			Compress     bool                   `toml:"compress"`
			Services     []ServerServiceForTOML `toml:"services"`
		}

		var tomlServices []ServerServiceForTOML
		for _, service := range cfg.Server.Services {
			tomlService := ServerServiceForTOML{
				Name:     service.Name,
				Target:   service.Target,
				Protocol: service.Protocol,
				Enabled:  service.Enabled,
			}
			if service.Password != "" {
				tomlService.Password = &service.Password
			}
			if service.Compress != nil {
				tomlService.Compress = service.Compress
			}
			tomlServices = append(tomlServices, tomlService)
		}

		serverConfig := struct {
			IsServer bool                `toml:"is_server"`
			Identity IdentityConfig      `toml:"identity"`
			Server   ServerConfigForTOML `toml:"server"`
			Common   CommonConfig        `toml:"common"`
		}{
			IsServer: cfg.IsServer,
			Identity: cfg.Identity,
			Server: ServerConfigForTOML{
				PasswordHash: cfg.Server.PasswordHash,
				Compress:     cfg.Server.Compress,
				Services:     tomlServices,
			},
			Common: cfg.Common,
		}
		if err := encoder.Encode(serverConfig); err != nil {
			return fmt.Errorf("failed to encode TOML server config: %v", err)
		}
	} else {

		type LocalServiceForTOML struct {
			Name     string   `toml:"name"`
			Local    string   `toml:"local"`
			Password *string  `toml:"password,omitempty"`
			Protocol []string `toml:"protocol"`
			Enabled  bool     `toml:"enabled"`
		}

		type ClientConfigForTOML struct {
			ServerID string                `toml:"server_id"`
			Services []LocalServiceForTOML `toml:"services"`
		}

		var tomlServices []LocalServiceForTOML
		for _, service := range cfg.Client.Services {
			tomlService := LocalServiceForTOML{
				Name:     service.Name,
				Local:    service.Local,
				Protocol: service.Protocol,
				Enabled:  service.Enabled,
			}

			if service.Password != "" {
				tomlService.Password = &service.Password
			}
			tomlServices = append(tomlServices, tomlService)
		}

		clientConfig := struct {
			IsServer bool                `toml:"is_server"`
			Identity IdentityConfig      `toml:"identity"`
			Client   ClientConfigForTOML `toml:"client"`
			Common   CommonConfig        `toml:"common"`
		}{
			IsServer: cfg.IsServer,
			Identity: cfg.Identity,
			Client: ClientConfigForTOML{
				ServerID: cfg.Client.ServerID,
				Services: tomlServices,
			},
			Common: cfg.Common,
		}
		if err := encoder.Encode(clientConfig); err != nil {
			return fmt.Errorf("failed to encode TOML client config: %v", err)
		}
	}

	var content strings.Builder
	writeTomlHeader(&content, cfg.IsServer)
	content.WriteString(buffer.String())

	return os.WriteFile(configPath, []byte(content.String()), 0644)
}

func writeTomlHeader(content *strings.Builder, isServer bool) {
	const prefix = `# F2P Configuration File (TOML Format)
# Generated automatically - you can modify the service configurations
# This is a `
	const suffix = ` configuration
#
# Documentation:
# - identity: Auto-generated, do not modify
# - services: You can modify these as needed
# - password: Optional service-level passwords
#   * To set a password: add 'password = "your_password"' to any service
#   * To remove a password: delete the password line completely
#   * Empty passwords ("") are treated as no password
# - server.compress: Enable zstd compression for data streams (server-side, default: true)
# - service.compress (server): Optional override per service (true/false). Omit to inherit.
# - common.zstd_level: Zstd compression level (default: 20 best, optional: 1..20)
# - common.zstd_min_size_b: Min size to attempt compression, pure number in BYTES. Default: 256. Range: 32..2048. Must be < chunk size.
# - common.zstd_chunk_size_kb: Chunk size for framing, pure number in KILOBYTES. Default: 32. Range: 8..128.

`
	role := "CLIENT"
	if isServer {
		role = "SERVER"
	}
	content.WriteString(prefix + role + suffix)
}

func createDefaultCommonConfig() CommonConfig {
	return CommonConfig{
		Protocol: "/f2p-forward/0.0.1",
		Listen: []string{
			"/ip4/0.0.0.0/tcp/0",
			"/ip6/::/tcp/0",
			"/ip4/0.0.0.0/udp/0/webrtc-direct",
			"/ip4/0.0.0.0/udp/0/quic-v1",
			"/ip4/0.0.0.0/udp/0/quic-v1/webtransport",
			"/ip6/::/udp/0/webrtc-direct",
			"/ip6/::/udp/0/quic-v1",
			"/ip6/::/udp/0/quic-v1/webtransport",
		},
		LogLevel:        "info",
		ZstdLevel:       20,
		ZstdMinSizeB:    256, // B
		ZstdChunkSizeKB: 32,  // KB
	}
}

// default server services example
func createDefaultServerServices() []ServiceConfig {
	return []ServiceConfig{
		{
			Name:     "ssh",
			Target:   "127.0.0.1:22",
			Password: "",
			Protocol: []string{"tcp"},
			Enabled:  false,
		},
		{
			Name:     "something",
			Target:   "127.0.0.1:5432",
			Password: "123",
			Protocol: []string{"tcp", "udp"},
			Compress: &True,
			Enabled:  false,
		},
	}
}

// default client services example
func createDefaultClientServices() []LocalServiceConfig {
	return []LocalServiceConfig{
		{
			Name:     "ssh",
			Local:    "127.0.0.1:2222",
			Password: "",
			Protocol: []string{"tcp"},
			Enabled:  false,
		},
		{
			Name:     "something",
			Local:    "127.0.0.1:15432",
			Password: "",
			Protocol: []string{"tcp", "udp"},
			Enabled:  false,
		},
	}
}

func GenerateInteractiveConfig(configPath string) error {
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	privKeyB64, peerID, err := GeneratePeerIdentity()
	if err != nil {
		return fmt.Errorf("failed to generate peer identity: %v", err)
	}
	fmt.Println("F2P Configuration Setup")
	fmt.Println()

	isServer := utils.AskYesNo("Are you setting up a server? (y/n): ")

	var cfg Config
	if isServer {
		var serverPassword string
		if utils.AskYesNo("Do you want to set a password for authentication? (y/n): ") {
			password := utils.AskPassword("Enter password: ")
			confirmPassword := utils.AskPassword("Confirm password: ")

			if password != confirmPassword {
				return fmt.Errorf("passwords do not match")
			}

			serverPassword = password
			fmt.Println("Password set successfully")
		} else {
			fmt.Println("No password set - authentication disabled")
		}

		var serverPasswordHash string
		if serverPassword != "" {
			serverPasswordHash = HashPassword(serverPassword)
		}

		cfg = Config{
			IsServer: true,
			Identity: IdentityConfig{
				PrivateKey: privKeyB64,
				PeerID:     peerID,
			},
			Server: ServerConfig{
				PasswordHash: serverPasswordHash,
				Compress:     true,
				Services:     createDefaultServerServices(),
			},
			Common: createDefaultCommonConfig(),
		}

		fmt.Printf("\nServer Peer ID: %s\n", peerID)
		fmt.Println("Give this Peer ID to clients to connect")

	} else {
		serverPeerID := askStringWithCancel("Enter server Peer ID: ")

		if len(serverPeerID) < 10 {
			fmt.Println("Invalid Peer ID: too short")
			fmt.Println("Configuration cancelled due to invalid input")
			os.Exit(1)
		}

		cfg = Config{
			IsServer: false,
			Identity: IdentityConfig{
				PrivateKey: privKeyB64,
				PeerID:     peerID,
			},
			Client: ClientConfig{
				ServerID: serverPeerID,
				Services: createDefaultClientServices(),
			},
			Common: createDefaultCommonConfig(),
		}

		fmt.Println("Note: You will be prompted for password when connecting to the server")
	}

	return Save(&cfg, configPath)
}

func HashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		timestamp := time.Now().UnixNano()
		salt = []byte(fmt.Sprintf("%016x", timestamp))
	}

	data := append(salt, []byte(password)...)
	hash := sha256.Sum256(data)

	saltB64 := base64.StdEncoding.EncodeToString(salt)
	hashB64 := base64.StdEncoding.EncodeToString(hash[:])

	return fmt.Sprintf("%s:%s", saltB64, hashB64)
}

func VerifyPassword(password, storedHash string) bool {
	if strings.Contains(storedHash, ":") {
		parts := strings.SplitN(storedHash, ":", 2)
		if len(parts) != 2 {
			return false
		}

		saltB64, expectedHashB64 := parts[0], parts[1]

		salt, err := base64.StdEncoding.DecodeString(saltB64)
		if err != nil {
			return false
		}

		data := append(salt, []byte(password)...)
		hash := sha256.Sum256(data)
		actualHashB64 := base64.StdEncoding.EncodeToString(hash[:])

		return actualHashB64 == expectedHashB64
	} else {
		hash := sha256.Sum256([]byte(password))
		oldHash := base64.StdEncoding.EncodeToString(hash[:])
		return oldHash == storedHash
	}
}

func askStringWithCancel(prompt string) string {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Reset(os.Interrupt, syscall.SIGTERM)

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print(prompt)

		type result struct {
			response string
			err      error
		}

		responseChan := make(chan result, 1)
		done := make(chan bool, 1)

		go func() {
			defer func() {
				done <- true
			}()
			input, err := reader.ReadString('\n')
			responseChan <- result{
				response: strings.TrimSpace(input),
				err:      err,
			}
		}()

		select {
		case res := <-responseChan:
			<-done
			if res.err != nil {
				fmt.Println("Configuration cancelled by user")
				os.Exit(1)
			}
			if res.response == "" {
				if utils.AskYesNo("No input provided. Do you want to cancel configuration? (y/n): ") {
					fmt.Println("Configuration cancelled by user")
					os.Exit(1)
				}
				continue
			}
			return res.response
		case <-sigChan:
			fmt.Println("\n\nConfiguration cancelled by user")
			<-done
			os.Exit(1)
			return ""
		}
	}
}

func GenerateDefaultTemplate(configPath string) error {
	return GenerateInteractiveConfig(configPath)
}

func validateProtocols(protocols []string, serviceName string) error {
	if len(protocols) == 0 {
		return fmt.Errorf("service '%s' must have at least one protocol", serviceName)
	}
	for _, protocol := range protocols {
		if protocol != "tcp" && protocol != "udp" {
			return fmt.Errorf("service '%s' protocol must be tcp or udp", serviceName)
		}
	}
	return nil
}

func (c *Config) ValidateConfig() error {
	if c.IsServer {
		if len(c.Server.Services) == 0 {
			return fmt.Errorf("server must have at least one service")
		}
		for _, service := range c.Server.Services {
			if service.Name == "" {
				return fmt.Errorf("service name cannot be empty")
			}
			if service.Target == "" {
				return fmt.Errorf("service '%s' target address cannot be empty", service.Name)
			}
			if err := validateProtocols(service.Protocol, service.Name); err != nil {
				return err
			}
		}
		if c.Identity.PrivateKey == "" || c.Identity.PeerID == "" {
			privKeyB64, peerID, err := GeneratePeerIdentity()
			if err != nil {
				return fmt.Errorf("failed to generate peer identity: %v", err)
			}
			c.Identity.PrivateKey = privKeyB64
			c.Identity.PeerID = peerID
		}

	} else {
		if c.Client.ServerID == "" {
			return fmt.Errorf("client server peer ID must be provided")
		}
		if len(c.Client.Services) == 0 {
			return fmt.Errorf("client must have at least one local service")
		}
		for _, service := range c.Client.Services {
			if service.Name == "" {
				return fmt.Errorf("remote service name cannot be empty")
			}
			if service.Local == "" {
				return fmt.Errorf("local address cannot be empty")
			}
			if err := validateProtocols(service.Protocol, service.Name); err != nil {
				return err
			}
		}
	}

	return nil
}
