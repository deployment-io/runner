package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/rpc"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/deployment-runner-kit/context_pack"
)

// A save to a server that accepts the connection but never answers returns the deadline error once
// its timeout passes, and leaves the shared connection alone.
func TestSaveInfraContext_StalledServerTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn // hold it open, never read or reply
		}
	}()
	defer func() {
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	}()

	shared, sharedServer := net.Pipe()
	defer sharedServer.Close()
	r := &RunnerClient{
		c:           rpc.NewClient(shared),
		isConnected: true,
		dial:        dialerFor(Options{Service: listener.Addr().String()}),
	}
	defer r.c.Close()

	start := time.Now()
	_, err = r.SaveInfraContext("org", "[]", 100*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("SaveInfraContext took %s against a stalled server, want ~100ms", elapsed)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
}

// fakeContextPacks stands in for deployment-server's ContextPacks service.
type fakeContextPacks struct {
	got chan context_pack.SaveInfraContextArgsV1
}

func (f *fakeContextPacks) SaveInfraContextV1(args *context_pack.SaveInfraContextArgsV1, reply *context_pack.SaveInfraContextReplyV1) error {
	f.got <- *args
	reply.Saved = 3
	return nil
}

// serveContextPacks serves a fake ContextPacks on l, wrapping each accepted connection (e.g. in TLS),
// one net/rpc ServeConn per connection as deployment-server does.
func serveContextPacks(t *testing.T, l net.Listener, wrap func(net.Conn) net.Conn) *fakeContextPacks {
	t.Helper()
	f := &fakeContextPacks{got: make(chan context_pack.SaveInfraContextArgsV1, 1)}
	srv := rpc.NewServer()
	if err := srv.RegisterName("ContextPacks", f); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go srv.ServeConn(wrap(conn))
		}
	}()
	return f
}

// A save reaches the server with the packs and the runner's token, and returns how many it stored.
func TestSaveInfraContext_RoundTrip(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	f := serveContextPacks(t, listener, func(c net.Conn) net.Conn { return c })

	r := &RunnerClient{isConnected: true, token: "tok", dial: dialerFor(Options{Service: listener.Addr().String()})}
	saved, err := r.SaveInfraContext("org", `[{"scope":{}}]`, 2*time.Second)
	if err != nil || saved != 3 {
		t.Fatalf("saved, err = %d, %v; want 3, nil", saved, err)
	}
	got := <-f.got
	if got.PacksJSON != `[{"scope":{}}]` || got.Token != "tok" || got.OrganizationID == "" {
		t.Errorf("server got %+v", got)
	}
}

// The production path: mutual TLS as deployment-server serves it (RequireAndVerifyClientCert), with
// the client's certificate and key given as PEMs whose newlines are escaped, as the runner's env
// carries them.
func TestSaveInfraContext_MutualTLSRoundTrip(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	// leaf returns a certificate chain (leaf + CA, as both sides' configs expect) and its key, in PEM.
	leaf := func(serial int64, server bool) (chainPEM, keyPEM []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "test-leaf"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if server {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		chainPEM = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
		return chainPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}
	serverChain, serverKey := leaf(2, true)
	clientChain, clientKey := leaf(3, false)

	serverPair, err := tls.X509KeyPair(serverChain, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(caCert)
	serverConfig := &tls.Config{Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	f := serveContextPacks(t, listener, func(c net.Conn) net.Conn { return tls.Server(c, serverConfig) })

	escaped := func(b []byte) string { return strings.ReplaceAll(string(b), "\n", `\n`) }
	r := &RunnerClient{isConnected: true, token: "tok", dial: dialerFor(Options{
		Service: listener.Addr().String(), ClientCertPem: escaped(clientChain), ClientKeyPem: escaped(clientKey),
	})}
	saved, err := r.SaveInfraContext("org", "[]", 3*time.Second)
	if err != nil || saved != 3 {
		t.Fatalf("saved, err = %d, %v; want 3, nil", saved, err)
	}
	<-f.got
}

// A refused connection fails the save straight away.
func TestSaveInfraContext_RefusedConnectionFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	r := &RunnerClient{isConnected: true, dial: dialerFor(Options{Service: addr})}
	start := time.Now()
	if _, err := r.SaveInfraContext("org", "[]", time.Second); err == nil {
		t.Fatal("SaveInfraContext succeeded against a closed port")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a refused save took %s", elapsed)
	}
}
