package holepunch

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"time"

	"github.com/quic-go/quic-go"
)

const DirectQUICALPN = "f2p-direct"

// SessionTokenLen is the length of the raw session token in bytes.
const SessionTokenLen = 16

// Stream type prefixes for multiplexing QUIC streams.
const (
	StreamTypeControl  byte = 0x01
	StreamTypeData     byte = 0x02
	StreamTypeDataZstd byte = 0x03
)

// GenerateSessionToken creates a random session token.
func GenerateSessionToken() ([]byte, error) {
	token := make([]byte, SessionTokenLen)
	_, err := rand.Read(token)
	return token, err
}

// generateSelfSignedTLS creates a self-signed TLS config for the QUIC server.
func generateSelfSignedTLS() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{certDER},
			PrivateKey:  key,
		}},
		NextProtos: []string{DirectQUICALPN},
	}, nil
}

// clientTLSConfig creates a TLS config for the QUIC client (skip server verification).
func clientTLSConfig() *tls.Config {
	return &tls.Config{
		NextProtos:         []string{DirectQUICALPN},
		InsecureSkipVerify: true,
	}
}

var defaultQUICConfig = &quic.Config{
	MaxIdleTimeout:  60 * time.Second,
	KeepAlivePeriod: 15 * time.Second,
}

// DirectDialQUIC establishes a QUIC connection through a punched socket and authenticates.
func DirectDialQUIC(ctx context.Context, punched *PunchedSocket, sessionToken []byte) (*quic.Conn, error) {
	tr := &quic.Transport{Conn: punched.Conn}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, err := tr.Dial(dialCtx, punched.RemoteAddr, clientTLSConfig(), defaultQUICConfig)
	if err != nil {
		return nil, fmt.Errorf("QUIC dial: %w", err)
	}

	// Authenticate: send session token on first stream
	authStream, err := conn.OpenStream()
	if err != nil {
		conn.CloseWithError(1, "open auth stream failed")
		return nil, fmt.Errorf("open auth stream: %w", err)
	}

	if _, err := authStream.Write(sessionToken); err != nil {
		conn.CloseWithError(1, "write token failed")
		return nil, fmt.Errorf("write token: %w", err)
	}

	// Wait for response
	resp := make([]byte, 1)
	authStream.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(authStream, resp); err != nil {
		conn.CloseWithError(1, "read auth response failed")
		return nil, fmt.Errorf("read auth response: %w", err)
	}
	authStream.Close()

	if resp[0] != 0x01 {
		conn.CloseWithError(1, "auth rejected")
		return nil, fmt.Errorf("authentication rejected by server")
	}

	slog.Info("Direct QUIC connection authenticated", "remote", punched.RemoteAddr)
	return conn, nil
}

// DirectListenQUIC starts a QUIC listener on the punched socket.
// Returns the transport and listener. Caller is responsible for closing them.
func DirectListenQUIC(punched *PunchedSocket) (*quic.Transport, *quic.Listener, error) {
	tlsConf, err := generateSelfSignedTLS()
	if err != nil {
		return nil, nil, fmt.Errorf("generate TLS: %w", err)
	}

	tr := &quic.Transport{Conn: punched.Conn}

	ln, err := tr.Listen(tlsConf, defaultQUICConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("QUIC listen: %w", err)
	}

	return tr, ln, nil
}

// AcceptAndAuthQUIC accepts a QUIC connection and verifies the session token.
func AcceptAndAuthQUIC(ctx context.Context, ln *quic.Listener, expectedToken []byte) (*quic.Conn, error) {
	acceptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := ln.Accept(acceptCtx)
	if err != nil {
		return nil, fmt.Errorf("accept QUIC: %w", err)
	}

	// Accept first stream (auth)
	streamCtx, streamCancel := context.WithTimeout(ctx, 10*time.Second)
	defer streamCancel()

	authStream, err := conn.AcceptStream(streamCtx)
	if err != nil {
		conn.CloseWithError(1, "no auth stream")
		return nil, fmt.Errorf("accept auth stream: %w", err)
	}

	token := make([]byte, SessionTokenLen)
	authStream.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(authStream, token); err != nil {
		conn.CloseWithError(1, "read token failed")
		return nil, fmt.Errorf("read token: %w", err)
	}

	if !bytes.Equal(token, expectedToken) {
		authStream.Write([]byte{0x00})
		authStream.Close()
		conn.CloseWithError(1, "bad token")
		return nil, fmt.Errorf("session token mismatch")
	}

	authStream.Write([]byte{0x01})
	authStream.Close()

	slog.Info("QUIC client authenticated successfully")
	return conn, nil
}
