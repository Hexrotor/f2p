package utils

const (
	appName = "f2p"
	version = "0.0.2"
	protocol = "f2p-forward/0.0.2"
)

func GetUserAgent() string {
	return appName + "/" + version
}

func GetProtocol() string {
	return protocol
}