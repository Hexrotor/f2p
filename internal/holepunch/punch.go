package holepunch

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"time"
)

// UDPSocketArray manages multiple UDP sockets for hole punching.
type UDPSocketArray struct {
	sockets []*net.UDPConn
	mu      sync.Mutex
}

// NewUDPSocketArray creates n UDP sockets bound to random ports.
func NewUDPSocketArray(n int) (*UDPSocketArray, error) {
	arr := &UDPSocketArray{
		sockets: make([]*net.UDPConn, 0, n),
	}
	for i := 0; i < n; i++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			arr.Close()
			return nil, fmt.Errorf("listen socket %d: %w", i, err)
		}
		arr.sockets = append(arr.sockets, conn)
	}
	return arr, nil
}

// SendAll sends data from all sockets to the given address.
func (a *UDPSocketArray) SendAll(data []byte, addr *net.UDPAddr) error {
	for _, sock := range a.sockets {
		sock.SetWriteDeadline(time.Now().Add(1 * time.Second))
		if _, err := sock.WriteTo(data, addr); err != nil {
			slog.Debug("punch send error", "local", sock.LocalAddr(), "remote", addr, "error", err)
		}
	}
	return nil
}

// Close closes all sockets.
func (a *UDPSocketArray) Close() {
	for _, s := range a.sockets {
		s.Close()
	}
}

// ListenForPunch listens on all sockets for incoming punch packets with the given TID.
// Returns the first socket that receives a matching packet and the remote address.
func (a *UDPSocketArray) ListenForPunch(tid uint32, timeout time.Duration) *PunchedSocket {
	type result struct {
		conn       *net.UDPConn
		remoteAddr *net.UDPAddr
	}
	resultCh := make(chan result, 1)
	done := make(chan struct{})
	defer close(done)

	for _, sock := range a.sockets {
		go func(s *net.UDPConn) {
			buf := make([]byte, 256)
			for {
				s.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				n, raddr, err := s.ReadFromUDP(buf)
				if err != nil {
					select {
					case <-done:
						return
					default:
						continue
					}
				}
				if rxTID, ok := ParsePunchPacket(buf[:n]); ok && rxTID == tid {
					select {
					case resultCh <- result{conn: s, remoteAddr: raddr}:
					default:
					}
					return
				}
			}
		}(sock)
	}

	select {
	case r := <-resultCh:
		return &PunchedSocket{
			Conn:       r.conn,
			LocalAddr:  r.conn.LocalAddr().(*net.UDPAddr),
			RemoteAddr: r.remoteAddr,
		}
	case <-time.After(timeout):
		return nil
	}
}

// PunchSymToCone performs the birthday attack from the Symmetric (client) side
// toward a Cone (server) peer.
//
// Flow:
//  1. Client creates N sockets, each sends to server's known public port
//  2. Server simultaneously sends to random ports on client's public IP
//  3. Any socket that receives server's packet → punched
func punchSymToCone(serverAddr *net.UDPAddr, tid uint32) (*PunchedSocket, error) {
	slog.Info("Starting SymToCone punch (birthday attack)", "server", serverAddr, "sockets", SymToConeSocketCount)

	arr, err := NewUDPSocketArray(SymToConeSocketCount)
	if err != nil {
		return nil, fmt.Errorf("create socket array: %w", err)
	}

	pkt := MakePunchPacket(tid)
	deadline := time.Now().Add(PunchTimeout)

	for round := 0; round < PunchMaxRounds && time.Now().Before(deadline); round++ {
		slog.Debug("Punch round", "round", round+1)

		// Send from all sockets to server
		roundEnd := time.Now().Add(PunchRoundDuration)
		for time.Now().Before(roundEnd) {
			arr.SendAll(pkt, serverAddr)
			// Check for response
			punched := arr.ListenForPunch(tid, PunchSendInterval)
			if punched != nil {
				// Close other sockets, keep punched one
				closeSockets(arr.sockets, punched.Conn)
				slog.Info("Punch succeeded (SymToCone)", "local", punched.LocalAddr, "remote", punched.RemoteAddr)
				return punched, nil
			}
		}
	}

	arr.Close()
	return nil, fmt.Errorf("punch timeout after %v", PunchTimeout)
}

// PunchConeToSym is the server (Cone) side of SymToCone.
// It sends packets to random ports on the client's public IP.
//
// serverSocket is the server's dedicated punch socket (fixed mapped port).
func punchConeToSym(serverSocket *net.UDPConn, clientIP net.IP, tid uint32) (*PunchedSocket, error) {
	slog.Info("Starting ConeToSym punch (server side)", "client_ip", clientIP)

	pkt := MakePunchPacket(tid)
	shuffledPorts := generateShuffledPorts()
	deadline := time.Now().Add(PunchTimeout)
	portIdx := 0

	// Also listen for incoming punch packets
	type recvResult struct {
		remoteAddr *net.UDPAddr
	}
	recvCh := make(chan recvResult, 1)

	go func() {
		buf := make([]byte, 256)
		for time.Now().Before(deadline) {
			serverSocket.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, raddr, err := serverSocket.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			if rxTID, ok := ParsePunchPacket(buf[:n]); ok && rxTID == tid {
				select {
				case recvCh <- recvResult{remoteAddr: raddr}:
				default:
				}
				return
			}
		}
	}()

	for round := 0; round < PunchMaxRounds && time.Now().Before(deadline); round++ {
		// How many random ports to try this round
		packetsThisRound := randomInt(BirthdayMinPackets, BirthdayMaxPackets)
		slog.Debug("Server punch round", "round", round+1, "packets", packetsThisRound)

		for i := 0; i < packetsThisRound && time.Now().Before(deadline); i++ {
			if portIdx >= len(shuffledPorts) {
				portIdx = 0
			}
			dstAddr := &net.UDPAddr{IP: clientIP, Port: shuffledPorts[portIdx]}
			portIdx++

			serverSocket.SetWriteDeadline(time.Now().Add(1 * time.Second))
			serverSocket.WriteTo(pkt, dstAddr)

			// Check for response every 100 packets
			if i%100 == 99 {
				select {
				case r := <-recvCh:
					slog.Info("Punch succeeded (ConeToSym server)", "remote", r.remoteAddr)
					return &PunchedSocket{
						Conn:       serverSocket,
						LocalAddr:  serverSocket.LocalAddr().(*net.UDPAddr),
						RemoteAddr: r.remoteAddr,
					}, nil
				default:
				}
			}
		}

		// Check at end of round
		select {
		case r := <-recvCh:
			slog.Info("Punch succeeded (ConeToSym server)", "remote", r.remoteAddr)
			return &PunchedSocket{
				Conn:       serverSocket,
				LocalAddr:  serverSocket.LocalAddr().(*net.UDPAddr),
				RemoteAddr: r.remoteAddr,
			}, nil
		case <-time.After(1 * time.Second):
		}
	}

	return nil, fmt.Errorf("server punch timeout")
}

// PunchBothEasySym performs port-prediction based punching for two Easy Symmetric NATs.
func punchBothEasySym(
	peerAddr *net.UDPAddr,
	myIncremental bool,
	peerIncremental bool,
	myCurrentPort int,
	tid uint32,
) (*PunchedSocket, error) {
	slog.Info("Starting BothEasySym punch", "peer", peerAddr, "my_inc", myIncremental, "peer_inc", peerIncremental)

	// Predict peer's next port
	predictedPeerPort := peerAddr.Port
	if peerIncremental {
		predictedPeerPort += EasySymPortOffset
	} else {
		predictedPeerPort -= EasySymPortOffset
	}
	targetAddr := &net.UDPAddr{IP: peerAddr.IP, Port: predictedPeerPort}

	arr, err := NewUDPSocketArray(BothEasySymSocketCount)
	if err != nil {
		return nil, fmt.Errorf("create socket array: %w", err)
	}

	pkt := MakePunchPacket(tid)
	deadline := time.Now().Add(PunchTimeout)

	for time.Now().Before(deadline) {
		arr.SendAll(pkt, targetAddr)
		punched := arr.ListenForPunch(tid, PunchSendInterval)
		if punched != nil {
			closeSockets(arr.sockets, punched.Conn)
			slog.Info("Punch succeeded (BothEasySym)", "local", punched.LocalAddr, "remote", punched.RemoteAddr)
			return punched, nil
		}
	}

	arr.Close()
	return nil, fmt.Errorf("both easy sym punch timeout")
}

// generateShuffledPorts returns a shuffled slice of ports 1-65535.
func generateShuffledPorts() []int {
	ports := make([]int, 65535)
	for i := range ports {
		ports[i] = i + 1
	}
	// Fisher-Yates shuffle
	for i := len(ports) - 1; i > 0; i-- {
		j := randomInt(0, i+1)
		ports[i], ports[j] = ports[j], ports[i]
	}
	return ports
}

func randomInt(min, max int) int {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(max-min)))
	return int(n.Int64()) + min
}

func closeSockets(sockets []*net.UDPConn, keep *net.UDPConn) {
	for _, s := range sockets {
		if s != keep {
			s.Close()
		}
	}
}

// PunchConeToCone performs a simple punch between two Cone NATs.
// Both sides know each other's stable public address.
func punchConeToCone(mySocket *net.UDPConn, peerAddr *net.UDPAddr, tid uint32) (*PunchedSocket, error) {
	slog.Info("Starting ConeToCone punch", "peer", peerAddr)

	pkt := MakePunchPacket(tid)
	deadline := time.Now().Add(PunchTimeout)

	// Listen in background
	type recvResult struct {
		remoteAddr *net.UDPAddr
	}
	recvCh := make(chan recvResult, 1)

	go func() {
		buf := make([]byte, 256)
		for time.Now().Before(deadline) {
			mySocket.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, raddr, err := mySocket.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			if rxTID, ok := ParsePunchPacket(buf[:n]); ok && rxTID == tid {
				select {
				case recvCh <- recvResult{remoteAddr: raddr}:
				default:
				}
				return
			}
		}
	}()

	// Send periodically
	for time.Now().Before(deadline) {
		mySocket.SetWriteDeadline(time.Now().Add(1 * time.Second))
		mySocket.WriteTo(pkt, peerAddr)

		select {
		case r := <-recvCh:
			slog.Info("Punch succeeded (ConeToCone)", "remote", r.remoteAddr)
			return &PunchedSocket{
				Conn:       mySocket,
				LocalAddr:  mySocket.LocalAddr().(*net.UDPAddr),
				RemoteAddr: r.remoteAddr,
			}, nil
		case <-time.After(PunchSendInterval):
		}
	}

	return nil, fmt.Errorf("cone-to-cone punch timeout")
}

// CreatePunchSocket creates a UDP socket and discovers its public mapping via STUN.
func CreatePunchSocket(stunServer string) (*net.UDPConn, *NATInfo, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, nil, fmt.Errorf("listen: %w", err)
	}
	mapped, err := stunQuery(conn, stunServer)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("STUN query: %w", err)
	}
	return conn, &NATInfo{
		PublicIP:   mapped.IP.String(),
		PublicPort: mapped.Port,
	}, nil
}

// ExecutePunch runs the appropriate punch strategy based on method and NAT types.
// punchSock may be nil; Symmetric-side punch functions create their own sockets.
func ExecutePunch(
	myNAT *NATInfo,
	peerNAT *NATInfo,
	method PunchMethod,
	tid uint32,
	punchSock *net.UDPConn,
) (*PunchedSocket, error) {
	peerAddr := &net.UDPAddr{
		IP:   net.ParseIP(peerNAT.PublicIP),
		Port: peerNAT.PublicPort,
	}

	switch method {
	case PunchConeToCone:
		if punchSock == nil {
			return nil, fmt.Errorf("ConeToCone requires a punch socket")
		}
		return punchConeToCone(punchSock, peerAddr, tid)

	case PunchSymToCone:
		if myNAT.Type.IsSymmetric() {
			// I am Symmetric, peer is Cone → birthday attack from my side
			return punchSymToCone(peerAddr, tid)
		}
		// I am Cone, peer is Symmetric → send to random ports on peer's IP
		if punchSock == nil {
			return nil, fmt.Errorf("ConeToSym requires a punch socket")
		}
		return punchConeToSym(punchSock, net.ParseIP(peerNAT.PublicIP), tid)

	case PunchEasySymToEasySym:
		return punchBothEasySym(
			peerAddr,
			myNAT.Type == NATSymmetricEasyInc,
			peerNAT.Type == NATSymmetricEasyInc,
			myNAT.PublicPort,
			tid,
		)

	default:
		return nil, fmt.Errorf("unsupported punch method: %d", method)
	}
}
