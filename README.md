# F2P

[[中文]](README_zh-cn.md)

[![Build test](https://github.com/Hexrotor/f2p/actions/workflows/testBuild.yml/badge.svg)](https://github.com/Hexrotor/f2p/actions/workflows/testBuild.yml) [![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

F2P is a remote port forwarding tool built on libp2p. It supports TCP + UDP and uses the Kademlia DHT for decentralized peer discovery; with IPv6 or UDP NAT hole punching it establishes direct connections, enabling port forwarding without public IP.

## Installation

Download from [Releases](https://github.com/Hexrotor/f2p/releases) or build from source.

### Build

This project uses CGO zstd for compression. Ensure you have working C toolchain.

#### Windows
1. Install [MinGW-w64](https://www.mingw-w64.org/) and add it to PATH.
2. Build:
   ```powershell
   $env:CGO_ENABLED=1; go build -ldflags="-s -w" -o build\f2p.exe .\cmd\f2p
   ```

#### Linux
1. Install toolchain:
   ```bash
   sudo apt update && sudo apt install build-essential
   ```
2. Build:
   ```bash
   CGO_ENABLED=1 go build -ldflags="-s -w" -o build/f2p ./cmd/f2p
   ```

## Working

This program follows a Client/Server architecture. The server and backend services reside in the same network environment. The client connects to the server over p2p to access backend services. Through the DHT, a client can find and dial the server via its PeerID. A password can be configured on the server; only authenticated clients can establish forwarding sessions.

### p2p Connection Principles

- Peer Discovery: libp2p (derived from IPFS) can join the IPFS DHT. In the DHT, a client resolves a server's `PeerID` to its `multiaddr` list, then attempts a connection.
- Connection Establishment: libp2p provides automatic relay and NAT hole punching. The server registers some peers as relays. A client often first connects via a relay multiaddr, then libp2p attempts to upgrade to a direct hole-punched connection. **Use [NatTypeTester](https://github.com/HMBSbige/NatTypeTester) to test whether your network environment supports NAT hole punching.**
- Current Limitation: If the server has no public IPv4 and also cannot be reached via IPv6 or hole punching, the final connection fails. libp2p limits relay usage, in my tests, without enabling `WithAllowLimitedConn` in code it can't freely open streams over relays. Relays appear to allow at most one stream and have limited bandwidth plus higher latency, so this project does not enable relayed streaming for now.

## Command Line

```
> f2p.exe -h
F2P - Service forwarding over p2p connection, based on libp2p

Usage:
  f2p [options] [config_file]

Options:
  --config, -c <file>      Specify config file (default: config.toml)
  --generate, -g           Generate new configuration interactively (Will overwrite existing config)
  --change-pwd             Change server password
  --regenerate-id, -r      Regenerate server identity (Will change server's peerID but keep other configs)
  --help, -h               Show this help message

Examples:
  f2p                      # Run with config.toml
  f2p server.toml          # Run with server.toml
```

## Configuration File

At startup the program checks for a configuration file (default `config.toml` in current directory). If it doesn't exist an interactive setupwizard starts. Please edit the config only from program generated.

### Config Example

```toml
is_server = true

[identity]
private_key = "..."
peer_id = "..."

[server]
password_hash = "..."
compress = true           # Enable zstd by default, controlled by server-side

[[server.services]]
name = "ssh"              # Service name; client must match this name with server
target = "127.0.0.1:22"   # Backend target
protocol = ["tcp"]
enabled = false

[[server.services]]
name = "something"
target = "127.0.0.1:5432"
password = "123"          # Optional per-service password
protocol = ["tcp", "udp"] # Multi-protocol support
compress = false          # Override compression at service level
enabled = false

[client]
server_id = "..."         # Server's peer ID

[[client.services]]
name = "ssh"
local = "127.0.0.1:2222"
protocol = ["tcp"]
enabled = false

[[client.services]]       # Client passively accepts server compression settings, no selection for now
name = "something"
local = "127.0.0.1:15432"
protocol = ["tcp", "udp"]
password = "123"
enabled = false

[common]
protocol = "/f2p-forward/0.0.1" # libp2p protocol, MUST match between server and client
listen = ["/ip4/0.0.0.0/tcp/0", "/ip6/::/tcp/0", "/ip4/0.0.0.0/udp/0/webrtc-direct", "/ip4/0.0.0.0/udp/0/quic-v1", "/ip4/0.0.0.0/udp/0/quic-v1/webtransport", "/ip6/::/udp/0/webrtc-direct", "/ip6/::/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1/webtransport"]
log_level = "info"
zstd_level = 20           # 1-20: higher = better compression + higher CPU
zstd_min_size_b = 256     # Minimum payload size to trigger compression
zstd_chunk_size_kb = 32   # Chunk size
```

# zstd

Considring p2p may have constrained bandwidth, zstd compression is used. The project chooses the CGO-based zstd implementation; pure Go variants retain large RAM until GC. Each data frame is evaluated—if compression yields larger output the raw data is sent instead (CPU spent is lost but generally reduced bandwidth). Default level is currently 20, may adjust for low-CPU devices later. Suggestions welcome via issue/PR.

## License

[LICENSE](LICENSE)