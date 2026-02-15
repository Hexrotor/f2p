package holepunch

import (
	"fmt"
	"net"
	"time"
)

// NATType represents the detected NAT type.
type NATType int

const (
	NATUnknown         NATType = iota
	NATOpen                    // No NAT / public IP
	NATCone                    // Full Cone / Restricted / Port Restricted (mapping is endpoint-independent)
	NATSymmetricEasyInc        // Symmetric with predictable incrementing ports
	NATSymmetricEasyDec        // Symmetric with predictable decrementing ports
	NATSymmetricHard           // Symmetric with random port allocation
)

func (n NATType) String() string {
	switch n {
	case NATOpen:
		return "Open"
	case NATCone:
		return "Cone"
	case NATSymmetricEasyInc:
		return "SymmetricEasyInc"
	case NATSymmetricEasyDec:
		return "SymmetricEasyDec"
	case NATSymmetricHard:
		return "SymmetricHard"
	default:
		return "Unknown"
	}
}

func (n NATType) IsCone() bool {
	return n == NATCone || n == NATOpen
}

func (n NATType) IsSymmetric() bool {
	return n == NATSymmetricEasyInc || n == NATSymmetricEasyDec || n == NATSymmetricHard
}

func (n NATType) IsEasySymmetric() bool {
	return n == NATSymmetricEasyInc || n == NATSymmetricEasyDec
}

// PunchMethod determines the hole punching strategy.
type PunchMethod int

const (
	PunchNone           PunchMethod = iota
	PunchConeToCone                 // Both sides are Cone, simple exchange
	PunchSymToCone                  // Birthday attack: Sym client, Cone server
	PunchEasySymToEasySym           // Port prediction: both Easy Symmetric
)

// DeterminePunchMethod chooses strategy based on both sides' NAT types.
// clientNAT is the initiator (f2p client), serverNAT is the responder (f2p server).
func DeterminePunchMethod(clientNAT, serverNAT NATType) PunchMethod {
	if clientNAT == NATUnknown || serverNAT == NATUnknown {
		return PunchNone
	}
	if clientNAT == NATOpen || serverNAT == NATOpen {
		return PunchNone // should already have direct connection
	}

	if clientNAT.IsCone() && serverNAT.IsCone() {
		return PunchConeToCone
	}
	if clientNAT.IsSymmetric() && serverNAT.IsCone() {
		return PunchSymToCone
	}
	if clientNAT.IsCone() && serverNAT.IsSymmetric() {
		return PunchSymToCone // reversed: server is Sym, client is Cone
	}
	if clientNAT.IsEasySymmetric() && serverNAT.IsEasySymmetric() {
		return PunchEasySymToEasySym
	}
	// HardSym <-> HardSym or HardSym <-> EasySym: can't punch
	return PunchNone
}

// NATInfo holds the detected NAT information for a peer.
type NATInfo struct {
	Type       NATType `json:"type"`
	PublicIP   string  `json:"public_ip"`
	PublicPort int     `json:"public_port"`
}

// SignalMsg is the message exchanged during hole punch signaling.
type SignalMsg struct {
	// Phase 1: NAT info exchange
	NATInfo *NATInfo `json:"nat_info,omitempty"`
	// Phase 2: Punch coordination
	PunchReady bool   `json:"punch_ready,omitempty"`
	Method     int    `json:"method,omitempty"`
	TID        uint32 `json:"tid,omitempty"`
	// Phase 3: Result
	Success      bool   `json:"success,omitempty"`
	SessionToken []byte `json:"session_token,omitempty"`
	// Error
	Error string `json:"error,omitempty"`
}

// PunchPacket is the UDP packet sent during hole punching.
// 4 bytes magic + 4 bytes TID + 56 bytes padding = 64 bytes total.
var PunchMagic = [4]byte{0xF2, 0x50, 0x48, 0x50} // "F2PHP" (F2P Hole Punch)

const PunchPacketSize = 64

func MakePunchPacket(tid uint32) []byte {
	pkt := make([]byte, PunchPacketSize)
	copy(pkt[0:4], PunchMagic[:])
	pkt[4] = byte(tid >> 24)
	pkt[5] = byte(tid >> 16)
	pkt[6] = byte(tid >> 8)
	pkt[7] = byte(tid)
	return pkt
}

func ParsePunchPacket(data []byte) (tid uint32, ok bool) {
	if len(data) < 8 {
		return 0, false
	}
	if data[0] != PunchMagic[0] || data[1] != PunchMagic[1] ||
		data[2] != PunchMagic[2] || data[3] != PunchMagic[3] {
		return 0, false
	}
	tid = uint32(data[4])<<24 | uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7])
	return tid, true
}

// PunchedSocket represents a successfully punched UDP socket.
type PunchedSocket struct {
	Conn       *net.UDPConn
	LocalAddr  *net.UDPAddr
	RemoteAddr *net.UDPAddr
}

func (p *PunchedSocket) String() string {
	return fmt.Sprintf("punched[%s <-> %s]", p.LocalAddr, p.RemoteAddr)
}

// Constants
const (
	// Number of UDP sockets for birthday attack (Sym -> Cone)
	SymToConeSocketCount = 84
	// Number of UDP sockets for both EasySym
	BothEasySymSocketCount = 25
	// Port prediction offset for EasySym
	EasySymPortOffset = 20
	// Max random ports to try per round (birthday attack)
	BirthdayMinPackets = 600
	BirthdayMaxPackets = 800
	// Punch timeout
	PunchTimeout = 30 * time.Second
	// Per-round send duration
	PunchRoundDuration = 5 * time.Second
	// Interval between sends
	PunchSendInterval = 100 * time.Millisecond
	// Max retry rounds
	PunchMaxRounds = 5

	// Confirmation duration after Cone side detects punch success
	// (sends punch packets back to confirmed address so Sym side can also detect)
	PunchConfirmDuration = 1 * time.Second
	PunchConfirmInterval = 50 * time.Millisecond

	// STUN timeout per request
	STUNTimeout = 3 * time.Second
)

// Default public STUN servers
var DefaultSTUNServers = []string{
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
	"stun.cloudflare.com:3478",
}
