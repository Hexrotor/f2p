# F2P

[![Build](https://github.com/Hexrotor/f2p/actions/workflows/release.yml/badge.svg)](https://github.com/Hexrotor/f2p/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

F2P 是一个基于 libp2p 的远程端口转发程序，支持 TCP+UDP，使用 Kademlia DHT 去中心化网络，无需公网 IP 设备即可实现端口转发。

## 安装

从 [Releases](https://github.com/Hexrotor/f2p/releases) 下载或自行编译

### 编译

本项目使用 CGO 以支持高性能 zstd 压缩，需确保本地有 C 编译环境。

#### Windows
1. 安装 [MinGW-w64](https://www.mingw-w64.org/) 并配置到 PATH。
2. 编译命令：
  ```powershell
  go build -ldflags="-s -w" -o build\f2p.exe .\cmd\f2p
  ```

#### Linux
1. 安装编译环境：
  ```bash
  sudo apt update && sudo apt install build-essential
  ```
2. 编译命令：
  ```bash
  go build -ldflags="-s -w" -o build/f2p ./cmd/f2p
  ```

## 工作模式

本程序为 C/S 架构，Server 和后端服务需要运行在同一网络环境中，Client 通过 p2p 连接到 Server 以访问其环境中的后端服务。DHT 网络使得客户端可以通过服务器 PeerID 查找并连接到服务器。服务器可设置连接密码，客户端认证通过后方可建立端口转发服务。

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

程序启动时会检查配置文件，默认为当前目录下的 `config.toml`。若配置文件不存在，会启动交互式创建流程；请由程序创建后再编辑 `services` 部分。

### 配置文件示例与说明

```toml
is_server = true

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

[client]                  # 客户端配置
server_id = "..."

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
protocol = "/f2p-forward/0.0.1"
listen = ["/ip4/0.0.0.0/tcp/0", "/ip6/::/tcp/0", "/ip4/0.0.0.0/udp/0/webrtc-direct", "/ip4/0.0.0.0/udp/0/quic-v1", "/ip4/0.0.0.0/udp/0/quic-v1/webtransport", "/ip6/::/udp/0/webrtc-direct", "/ip6/::/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1/webtransport"]  # libp2p multiaddr 监听地址，按需改动，端口 0 表示随机
log_level = "info"
zstd_level = 20           # zstd 压缩等级 1-20，后续可能会调整默认值
zstd_min_size_b = 256     # 压缩开启阈值，默认 256 字节
zstd_chunk_size_kb = 32   # 压缩分块大小
```

# zstd

考虑到 p2p 通信的带宽可能并不理想，本项目引入了 zstd 压缩传输数据，经测试选择了使用 CGO zstd，纯 Go 实现会有很高的内存占用，GC 才会被释放。代码中会判断压缩后的数据大小，如果比原数据大就会发送原数据，但刚刚的 CPU 压缩开销浪费了。默认压缩等级现在为最高级 20，后续可能考虑为低性能设备更改默认值。

## License

[LICENSE](LICENSE)