package holepunch

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/Hexrotor/f2p/internal/version"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// writeSignalMsg writes a length-prefixed JSON message.
func writeSignalMsg(w io.Writer, msg *SignalMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	// 4-byte big-endian length prefix
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := w.Write(lenBuf); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// readSignalMsg reads a length-prefixed JSON message.
func readSignalMsg(r io.Reader) (*SignalMsg, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	if msgLen > 1<<16 { // sanity limit: 64KB
		return nil, fmt.Errorf("message too large: %d", msgLen)
	}
	data := make([]byte, msgLen)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	var msg SignalMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &msg, nil
}

// ClientSignaling initiates hole punch signaling with the server over a libp2p stream.
// Returns the server's NAT info, session token, punch method, and transaction ID.
func ClientSignaling(ctx context.Context, h host.Host, serverID peer.ID, myNAT *NATInfo, baseProtocol string) (*NATInfo, []byte, PunchMethod, uint32, error) {
	// Open signaling stream with AllowLimitedConn (works over relay)
	ctx = network.WithAllowLimitedConn(ctx, "f2p-holepunch")
	stream, err := h.NewStream(ctx, serverID, protocol.ID(version.SignalingProtocol(baseProtocol)))
	if err != nil {
		return nil, nil, PunchNone, 0, fmt.Errorf("open signaling stream: %w", err)
	}
	defer stream.Close()

	// Generate TID
	tidBytes := make([]byte, 4)
	if _, err := rand.Read(tidBytes); err != nil {
		return nil, nil, PunchNone, 0, fmt.Errorf("generate tid: %w", err)
	}
	tid := binary.BigEndian.Uint32(tidBytes)

	// Send our NAT info
	if err := writeSignalMsg(stream, &SignalMsg{
		NATInfo: myNAT,
		TID:     tid,
	}); err != nil {
		return nil, nil, PunchNone, 0, fmt.Errorf("send NAT info: %w", err)
	}

	slog.Info("Sent NAT info to server", "type", myNAT.Type, "public", fmt.Sprintf("%s:%d", myNAT.PublicIP, myNAT.PublicPort))

	// Read server's response (NAT info + session token + method)
	resp, err := readSignalMsg(stream)
	if err != nil {
		return nil, nil, PunchNone, 0, fmt.Errorf("read server response: %w", err)
	}
	if resp.Error != "" {
		return nil, nil, PunchNone, 0, fmt.Errorf("server error: %s", resp.Error)
	}
	if resp.NATInfo == nil {
		return nil, nil, PunchNone, 0, fmt.Errorf("server did not send NAT info")
	}
	if len(resp.SessionToken) == 0 {
		return nil, nil, PunchNone, 0, fmt.Errorf("server did not send session token")
	}

	method := PunchMethod(resp.Method)
	tokenPreview := fmt.Sprintf("%x", resp.SessionToken[:min(4, len(resp.SessionToken))])
	slog.Info("Received server NAT info",
		"type", resp.NATInfo.Type,
		"public", fmt.Sprintf("%s:%d", resp.NATInfo.PublicIP, resp.NATInfo.PublicPort),
		"method", method,
		"token", tokenPreview+"...")

	// Wait for server's "ready" signal (server has started its punch goroutine)
	ready, err := readSignalMsg(stream)
	if err != nil {
		return nil, nil, PunchNone, 0, fmt.Errorf("read ready signal: %w", err)
	}
	if !ready.PunchReady {
		return nil, nil, PunchNone, 0, fmt.Errorf("server not ready: %s", ready.Error)
	}

	return resp.NATInfo, resp.SessionToken, method, tid, nil
}

// ServerSignalingHandler handles an incoming signaling stream from a client.
//
// cachedNATType is the server's detected NAT type (from DetectNAT at startup).
// cachedNATInfo is the full NAT info for fallback (used when server is Symmetric).
// stunServer is the STUN server to use for creating punch sockets.
// sessionToken is the token for QUIC authentication.
//
// onPunchReady is called after the server has sent its NAT info to the client
// but before sending the "ready" signal. The caller should start punch + QUIC
// goroutines in onPunchReady. Arguments: punchSock (may be nil for Sym server),
// server's NATInfo used in signaling, clientNAT, method, tid.
func ServerSignalingHandler(
	stream network.Stream,
	cachedNATType NATType,
	cachedNATInfo *NATInfo,
	stunServer string,
	sessionToken []byte,
	onPunchReady func(punchSock *net.UDPConn, myNAT *NATInfo, clientNAT *NATInfo, method PunchMethod, tid uint32) error,
) error {
	defer stream.Close()

	// Read client's NAT info
	msg, err := readSignalMsg(stream)
	if err != nil {
		return fmt.Errorf("read client NAT info: %w", err)
	}
	if msg.NATInfo == nil {
		_ = writeSignalMsg(stream, &SignalMsg{Error: "missing NAT info"})
		return fmt.Errorf("client did not send NAT info")
	}

	clientNAT := msg.NATInfo
	tid := msg.TID
	slog.Info("Received client NAT info",
		"type", clientNAT.Type,
		"public", fmt.Sprintf("%s:%d", clientNAT.PublicIP, clientNAT.PublicPort),
		"tid", tid)

	// Determine punch method
	method := DeterminePunchMethod(clientNAT.Type, cachedNATType)
	if method == PunchNone {
		_ = writeSignalMsg(stream, &SignalMsg{Error: fmt.Sprintf("no viable punch method: client=%s server=%s", clientNAT.Type, cachedNATType)})
		return fmt.Errorf("no viable punch method: client=%s server=%s", clientNAT.Type, cachedNATType)
	}

	// Create punch socket and determine server's NATInfo for this session.
	// For Cone server: create a dedicated socket and STUN-query it.
	// For Sym server: use cached NATInfo (actual ports created inside punch functions).
	var punchSock *net.UDPConn
	var myNAT *NATInfo

	if cachedNATType.IsCone() || cachedNATType == NATOpen {
		punchSock, myNAT, err = CreatePunchSocket(stunServer)
		if err != nil {
			_ = writeSignalMsg(stream, &SignalMsg{Error: fmt.Sprintf("create punch socket: %v", err)})
			return fmt.Errorf("create punch socket: %w", err)
		}
		myNAT.Type = cachedNATType
		slog.Info("Created punch socket", "public", fmt.Sprintf("%s:%d", myNAT.PublicIP, myNAT.PublicPort))
	} else {
		// Symmetric server: use cached info
		myNAT = &NATInfo{
			Type:       cachedNATType,
			PublicIP:   cachedNATInfo.PublicIP,
			PublicPort: cachedNATInfo.PublicPort,
		}
	}

	// Send our NAT info + method + session token
	if err := writeSignalMsg(stream, &SignalMsg{
		NATInfo:      myNAT,
		Method:       int(method),
		SessionToken: sessionToken,
		TID:          tid,
	}); err != nil {
		if punchSock != nil {
			punchSock.Close()
		}
		return fmt.Errorf("send NAT info: %w", err)
	}

	// Let caller prepare (start punch + QUIC goroutines)
	if err := onPunchReady(punchSock, myNAT, clientNAT, method, tid); err != nil {
		_ = writeSignalMsg(stream, &SignalMsg{Error: fmt.Sprintf("server punch setup failed: %v", err)})
		if punchSock != nil {
			punchSock.Close()
		}
		return fmt.Errorf("onPunchReady: %w", err)
	}

	// Signal ready to client
	if err := writeSignalMsg(stream, &SignalMsg{PunchReady: true}); err != nil {
		return fmt.Errorf("send ready: %w", err)
	}

	slog.Info("Signaling complete, punch started", "method", method, "tid", tid)
	return nil
}
