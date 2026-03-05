package version

import "strings"

const (
	AppName  = "f2p"
	Version  = "0.0.3"
	Protocol = "f2p-forward/0.0.2"
)

func UserAgent() string {
	return AppName + "/" + Version
}

func GetProtocol() string {
	return Protocol
}

// normalizeBase ensures the base has a leading slash and no trailing slash.
func normalizeBase(base string) string {
	if base == "" {
		return "/" + Protocol
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	return strings.TrimRight(base, "/")
}

func ControlProtocol(base string) string {
	return normalizeBase(base) + "/control"
}

func DataProtocol(base string) string {
	return normalizeBase(base) + "/data"
}

func DataProtocolZstd(base string) string {
	return normalizeBase(base) + "/data+zstd"
}

func SignalingProtocol(base string) string {
	return normalizeBase(base) + "/holepunch"
}
