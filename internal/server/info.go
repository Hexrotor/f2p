package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/Hexrotor/f2p/internal/message"
)

func (s *Server) printServerInfo() {
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("F2P Server Ready!")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Node ID: %s (Read full from your config file)\n", s.host.ID().ShortString())
	fmt.Printf("Password Required: %v\n", s.config.Server.PasswordHash != "")

	fmt.Println("\nAvailable Services:")
	for _, service := range s.config.Server.Services {
		serviceStatus := "Disabled"
		if service.Enabled {
			serviceStatus = "Enabled"
		}
		fmt.Printf("   * %s - %s\n", service.Name, serviceStatus)

		if service.Enabled {
			protocolsStr := strings.Join(service.Protocol, ", ")
			fmt.Printf("     ├─ Target: %s\n", service.Target)
			fmt.Printf("     └─ Protocols: %s\n", protocolsStr)
		}
	}

	fmt.Println(strings.Repeat("=", 60) + "\n")
}

func (s *Server) notifyClientsServerShutdown() {
	fmt.Println("Notifying all connected clients about server shutdown...")

	s.clientsMutex.RLock()
	clients := make([]*ClientSession, 0, len(s.authenticatedClients))
	for _, cs := range s.authenticatedClients {
		clients = append(clients, cs)
	}
	s.clientsMutex.RUnlock()

	if len(clients) == 0 {
		fmt.Println("No connected clients to notify")
		return
	}

	fmt.Printf("Sending shutdown notification to %d client(s)...\n", len(clients))

	for _, cs := range clients {
		go func(cs *ClientSession) {
			if cs.controlStream != nil {
				h := message.NewMessager(cs.controlStream, s.config, cs.peerID)
				_ = h.SendServerShutdownNotification()
				time.Sleep(100 * time.Millisecond)
				_ = cs.controlStream.Close()
			}
		}(cs)
	}

	time.Sleep(500 * time.Millisecond)
	fmt.Println("All shutdown notifications sent")
}
