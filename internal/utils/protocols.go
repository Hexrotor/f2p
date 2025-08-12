package utils

import "strings"

// Ensure base has leading slash and no trailing slash
func normalizeBase(base string) string {
	if base == "" {
		return "/f2p/0.0.1"
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	return strings.TrimRight(base, "/")
}

func ControlProtocol(base string) string {
	b := normalizeBase(base)
	return b + "/control"
}

func DataProtocol(base string) string {
	b := normalizeBase(base)
	return b + "/data"
}

func DataProtocolZstd(base string) string {
	b := normalizeBase(base)
	return b + "/data+zstd"
}
