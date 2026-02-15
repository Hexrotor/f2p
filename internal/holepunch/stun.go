package holepunch

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"time"
)

// STUN constants (RFC 5389)
const (
	stunMagicCookie    = 0x2112A442
	stunBindingRequest = 0x0001
	stunBindingSuccess = 0x0101
	stunAttrMappedAddr = 0x0001
	stunAttrXorMapped  = 0x0020
	stunHeaderSize     = 20
)

// stunMappedAddr is the result of a single STUN query.
type stunMappedAddr struct {
	IP   net.IP
	Port int
}

// buildSTUNBindingRequest creates a minimal STUN Binding Request packet.
func buildSTUNBindingRequest() ([]byte, [12]byte) {
	pkt := make([]byte, stunHeaderSize)
	// Message type: Binding Request
	binary.BigEndian.PutUint16(pkt[0:2], stunBindingRequest)
	// Message length: 0 (no attributes)
	binary.BigEndian.PutUint16(pkt[2:4], 0)
	// Magic cookie
	binary.BigEndian.PutUint32(pkt[4:8], stunMagicCookie)
	// Transaction ID (12 bytes random)
	var txID [12]byte
	rand.Read(txID[:])
	copy(pkt[8:20], txID[:])
	return pkt, txID
}

// parseSTUNBindingResponse extracts the mapped address from a STUN Binding Success Response.
func parseSTUNBindingResponse(data []byte, txID [12]byte) (*stunMappedAddr, error) {
	if len(data) < stunHeaderSize {
		return nil, fmt.Errorf("response too short: %d bytes", len(data))
	}

	msgType := binary.BigEndian.Uint16(data[0:2])
	if msgType != stunBindingSuccess {
		return nil, fmt.Errorf("unexpected STUN message type: 0x%04x", msgType)
	}

	// Verify magic cookie
	cookie := binary.BigEndian.Uint32(data[4:8])
	if cookie != stunMagicCookie {
		return nil, fmt.Errorf("invalid magic cookie: 0x%08x", cookie)
	}

	// Verify transaction ID
	for i := 0; i < 12; i++ {
		if data[8+i] != txID[i] {
			return nil, fmt.Errorf("transaction ID mismatch")
		}
	}

	msgLen := binary.BigEndian.Uint16(data[2:4])
	if len(data) < stunHeaderSize+int(msgLen) {
		return nil, fmt.Errorf("truncated response")
	}

	// Parse attributes, prefer XOR-MAPPED-ADDRESS over MAPPED-ADDRESS
	var mapped *stunMappedAddr
	pos := stunHeaderSize
	end := stunHeaderSize + int(msgLen)
	for pos+4 <= end {
		attrType := binary.BigEndian.Uint16(data[pos : pos+2])
		attrLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		pos += 4

		if pos+attrLen > end {
			break
		}

		switch attrType {
		case stunAttrXorMapped:
			if m := parseXorMappedAddress(data[pos:pos+attrLen], data[4:8], data[8:20]); m != nil {
				return m, nil // prefer XOR-MAPPED-ADDRESS
			}
		case stunAttrMappedAddr:
			if mapped == nil {
				mapped = parseMappedAddress(data[pos : pos+attrLen])
			}
		}

		// Attributes are padded to 4-byte boundary
		pos += attrLen
		if pad := attrLen % 4; pad != 0 {
			pos += 4 - pad
		}
	}

	if mapped != nil {
		return mapped, nil
	}
	return nil, fmt.Errorf("no mapped address in STUN response")
}

func parseXorMappedAddress(data, magicBytes, txID []byte) *stunMappedAddr {
	if len(data) < 8 {
		return nil
	}
	family := data[1]
	if family != 0x01 { // IPv4 only for now
		return nil
	}
	xport := binary.BigEndian.Uint16(data[2:4])
	port := xport ^ uint16(binary.BigEndian.Uint32(magicBytes)>>16)

	xip := make([]byte, 4)
	copy(xip, data[4:8])
	magic := binary.BigEndian.Uint32(magicBytes)
	ipVal := binary.BigEndian.Uint32(xip) ^ magic
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, ipVal)

	return &stunMappedAddr{IP: ip, Port: int(port)}
}

func parseMappedAddress(data []byte) *stunMappedAddr {
	if len(data) < 8 {
		return nil
	}
	family := data[1]
	if family != 0x01 { // IPv4
		return nil
	}
	port := binary.BigEndian.Uint16(data[2:4])
	ip := net.IP(data[4:8])
	return &stunMappedAddr{IP: ip, Port: int(port)}
}

// stunQuery sends a STUN Binding Request to the server and returns the mapped address.
func stunQuery(conn *net.UDPConn, serverAddr string) (*stunMappedAddr, error) {
	addr, err := net.ResolveUDPAddr("udp4", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", serverAddr, err)
	}

	pkt, txID := buildSTUNBindingRequest()

	conn.SetWriteDeadline(time.Now().Add(STUNTimeout))
	if _, err := conn.WriteTo(pkt, addr); err != nil {
		return nil, fmt.Errorf("write to %s: %w", serverAddr, err)
	}

	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(STUNTimeout))
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, fmt.Errorf("read from %s: %w", serverAddr, err)
	}

	return parseSTUNBindingResponse(buf[:n], txID)
}

// DetectNAT detects the NAT type by querying multiple STUN servers.
// Returns the NATType and the public address (IP:Port) as seen by the first STUN server.
func DetectNAT(stunServers []string) (*NATInfo, error) {
	if len(stunServers) < 2 {
		return nil, fmt.Errorf("need at least 2 STUN servers, got %d", len(stunServers))
	}

	// Socket 1: query all STUN servers from the same local port
	sock1, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}
	defer sock1.Close()

	var results []stunMappedAddr
	var publicIPs []string

	for _, server := range stunServers {
		mapped, err := stunQuery(sock1, server)
		if err != nil {
			slog.Debug("STUN query failed", "server", server, "error", err)
			continue
		}
		slog.Debug("STUN response", "server", server, "mapped", fmt.Sprintf("%s:%d", mapped.IP, mapped.Port))
		results = append(results, *mapped)
		publicIPs = append(publicIPs, mapped.IP.String())
	}

	if len(results) < 2 {
		if len(results) == 1 {
			return &NATInfo{
				Type:       NATUnknown,
				PublicIP:   results[0].IP.String(),
				PublicPort: results[0].Port,
			}, fmt.Errorf("only got response from 1 STUN server (need 2+)")
		}
		return nil, fmt.Errorf("no STUN responses received")
	}

	// Check if public IP varies (multi-homed or very unusual NAT)
	uniqueIPs := uniqueStrings(publicIPs)

	// Check if all mapped ports are the same → Cone NAT
	allSamePort := true
	basePort := results[0].Port
	for _, r := range results[1:] {
		if r.Port != basePort {
			allSamePort = false
			break
		}
	}

	info := &NATInfo{
		PublicIP:   results[0].IP.String(),
		PublicPort: results[0].Port,
	}

	if allSamePort {
		// Endpoint-independent mapping → Cone
		info.Type = NATCone
		slog.Info("NAT detected", "type", info.Type, "public", fmt.Sprintf("%s:%d", info.PublicIP, info.PublicPort))
		return info, nil
	}

	// Ports differ → Symmetric NAT. Check if it's Easy or Hard.
	if len(uniqueIPs) > 1 {
		// Different public IPs → Hard symmetric (or multi-NAT)
		info.Type = NATSymmetricHard
		slog.Info("NAT detected", "type", info.Type, "reason", "multiple public IPs")
		return info, nil
	}

	// Check port increment pattern using extra socket test
	ports := make([]int, len(results))
	for i, r := range results {
		ports[i] = r.Port
	}
	sort.Ints(ports)

	maxPortDiff := ports[len(ports)-1] - ports[0]
	if maxPortDiff > 100 {
		info.Type = NATSymmetricHard
		slog.Info("NAT detected", "type", info.Type, "port_range", maxPortDiff)
		return info, nil
	}

	// Extra bind test: new socket to same STUN server, check port pattern
	sock2, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		info.Type = NATSymmetricHard
		return info, nil
	}
	defer sock2.Close()

	extraMapped, err := stunQuery(sock2, stunServers[0])
	if err != nil {
		info.Type = NATSymmetricHard
		return info, nil
	}

	// Compare extra port with the range from sock1
	maxPort := ports[len(ports)-1]
	minPort := ports[0]

	incDiff := extraMapped.Port - maxPort
	decDiff := minPort - extraMapped.Port

	if incDiff > 0 && incDiff < 100 {
		info.Type = NATSymmetricEasyInc
		info.PublicPort = extraMapped.Port // use latest mapping as base
	} else if decDiff > 0 && decDiff < 100 {
		info.Type = NATSymmetricEasyDec
		info.PublicPort = extraMapped.Port
	} else {
		info.Type = NATSymmetricHard
	}

	slog.Info("NAT detected", "type", info.Type, "public", fmt.Sprintf("%s:%d", info.PublicIP, info.PublicPort))
	return info, nil
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
