# F2P

[![Build test](https://github.com/Hexrotor/f2p/actions/workflows/testBuild.yml/badge.svg)](https://github.com/Hexrotor/f2p/actions/workflows/testBuild.yml) [![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

F2P 是一个基于 libp2p 的远程端口转发程序，支持 TCP+UDP，使用 Kademlia DHT 去中心化网络实现节点发现，配合 IPv6 或 UDP NAT 打洞建立直连，无需依赖公网 IP 即可实现端口转发。

## 安装

从 [Releases](https://github.com/Hexrotor/f2p/releases) 下载或自行编译

### 编译

本项目使用 CGO 以支持高性能 zstd 压缩，需确保本地有 C 编译环境。

#### Windows
1. 安装 [MinGW-w64](https://www.mingw-w64.org/) 并配置到 PATH。
2. 编译：
  ```powershell
  $env:CGO_ENABLED=1; go build -ldflags="-s -w" -o build\f2p.exe .\cmd\f2p
  ```

#### Linux
1. 安装编译环境：
  ```bash
  sudo apt update && sudo apt install build-essential
  ```
2. 编译：
  ```bash
  CGO_ENABLED=1 go build -ldflags="-s -w" -o build/f2p ./cmd/f2p
  ```

## 工作模式

本程序为 C/S 架构，Server 和后端服务需要运行在同一网络环境中，Client 通过 p2p 连接到 Server 以访问其环境中的后端服务。DHT 网络使得客户端可以通过服务器 PeerID 查找并连接到服务器。服务器可设置连接密码，客户端认证通过后方可建立端口转发服务。

### p2p 连接原理

- 主机发现：libp2p 由 IPFS 项目衍生而来，可以直接接入 IPFS 的 DHT 网络，在 DHT 网络中客户端可以通过 `PeerID` 获取到对应 peer 的 `multiaddr` 从而发起连接
- 建立连接：libp2p 制定了一套自动中继机制与 NAT 打洞机制，本程序服务端会自动注册一批 peer 用于自身的中继。客户端在 DHT 网络中寻找服务端时，往往会先得到服务端的中继地址先建立中继连接，随后 libp2p 会尝试将其升级为打洞直连。**使用 [NatTypeTester](https://github.com/HMBSbige/NatTypeTester) 测试你的网络环境是否能 NAT 打洞成功。**
- 目前的问题：若服务端既没有公网 IPv4，也不能通过 IPv6 或打洞实现直连，则最终连接会失败。因为 libp2p 规定了中继能提供的服务是有限的，如果不在代码中显式指定 `WithAllowLimitedConn`，就不能使用中继建立 stream。经过测试，中继似乎最多允许一个 stream，并且中继提供的流量转发是有限的，局限性太大并且延迟高，故暂时不考虑启用。

## 命令参数

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

## 配置文件

程序启动时会检查配置文件。若配置文件不存在，会启动交互式创建流程。**请直接修改程序生成的配置文件，勿从此页复制。**

### 配置文件示例与说明

服务器:

```toml
is_server = true          # 指示程序作为服务器还是客户端运行

[identity]
private_key = "..."
peer_id = "..."

[server]
password_hash = "..."
compress = true           # 默认启用 zstd 压缩，由服务器控制

[[server.services]]
name = "ssh"              # 服务名，客户端配的时候必须与服务器一致
target = "127.0.0.1:22"   # 服务器目标后端服务
protocol = ["tcp"]
enabled = false

[[server.services]]
name = "something"
target = "127.0.0.1:5432"
password = "123"          # 支持设置服务级密码
protocol = ["tcp", "udp"] # 支持多协议
compress = false          # 单服务压缩选项可覆盖
enabled = false

[common]
protocol = "/f2p-forward/0.0.2" # libp2p 提供的协议功能，用于区分服务。确保服务器与客户端保持一致。
# libp2p multiaddr 监听地址，按需改动，端口 0 表示随机
listen = ["/ip4/0.0.0.0/tcp/0", "/ip6/::/tcp/0", "/ip4/0.0.0.0/udp/0/webrtc-direct", "/ip4/0.0.0.0/udp/0/quic-v1", "/ip4/0.0.0.0/udp/0/quic-v1/webtransport", "/ip6/::/udp/0/webrtc-direct", "/ip6/::/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1/webtransport"]
log_level = "info"
zstd_level = 20           # zstd 压缩等级 1-20，越大压缩率越高，但 CPU 消耗也越多，后续可能会考虑调整默认值
zstd_min_size_b = 256     # 压缩开启阈值，默认 256 字节
zstd_chunk_size_kb = 32   # 压缩分块大小
```

客户端:

```toml
is_server = false

[identity]
private_key = "..."
peer_id = "..."

[client]                  # 客户端配置
server_id = "..."         # 服务器 PeerID，交互式生成配置时填写

[[client.services]]
name = "ssh"
local = "127.0.0.1:2222"
protocol = ["tcp"]
enabled = false

[[client.services]]       # 客户端只能被动接收服务器的压缩设定，暂时不能自己调整
name = "something"
local = "127.0.0.1:15432"
protocol = ["tcp", "udp"]
password = "123"
enabled = false

[common]
protocol = "/f2p-forward/0.0.2" # libp2p 提供的协议功能，用于区分服务。确保服务器与客户端保持一致。
# libp2p multiaddr 监听地址，按需改动，端口 0 表示随机
listen = ["/ip4/0.0.0.0/tcp/0", "/ip6/::/tcp/0", "/ip4/0.0.0.0/udp/0/webrtc-direct", "/ip4/0.0.0.0/udp/0/quic-v1", "/ip4/0.0.0.0/udp/0/quic-v1/webtransport", "/ip6/::/udp/0/webrtc-direct", "/ip6/::/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1/webtransport"]
log_level = "info"
zstd_level = 20           # zstd 压缩等级 1-20，越大压缩率越高，但 CPU 消耗也越多，后续可能会考虑调整默认值
zstd_min_size_b = 256     # 压缩开启阈值，默认 256 字节
zstd_chunk_size_kb = 32   # 压缩分块大小
```

# zstd

考虑到 p2p 通信的带宽可能并不理想，本项目引入了 zstd 压缩传输数据。经测试选择了使用 CGO zstd，因为纯 Go 的 zstd 实现会有很高的内存占用，直到 GC 才会被释放。数据帧压缩是动态判断的，如果压缩后比原数据大就会选择发送原数据。这是为了确保压缩功能使网络开销只降不增，但刚刚压缩过程的 CPU 压缩开销就浪费了，如果您发现有更好的策略，欢迎提交 issue/pr。默认压缩等级现在为最高级 20，考虑到可能有低性能设备，后续可能更改默认值。

## License

[LICENSE](LICENSE)