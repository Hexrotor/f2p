package proto

// IsShutdownMessage checks whether the message is a shutdown notification from either side.
func (msg *UnifiedMessage) IsShutdownMessage() bool {
	if msg == nil {
		return false
	}
	t := msg.GetType()
	return t == MessageType_SERVER_SHUTDOWN || t == MessageType_CLIENT_SHUTDOWN
}
