package utils

const (
	appName = "f2p"
	version = "0.0.1-alpha"
)

func GetUserAgent() string {
	return appName + "/" + version
}