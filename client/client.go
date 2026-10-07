package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner-kit/types"
	"log"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"time"
)

type RunnerClient struct {
	sync.Mutex
	c           *rpc.Client
	isConnected bool
	isStarted   bool
	//organizationID     string
	token              string
	currentDockerImage string
	runnerRegion       string
	cloudAccountID     string
	runnerMode         runner_enums.Mode
	targetCloud        runner_enums.TargetCloud
	userID             string
	// dial opens a new connection to deployment-server with the options of the last connect, for
	// calls that need their own deadline-bounded connection (see SaveInfraContext).
	dial func(timeout time.Duration) (net.Conn, error)
}

func getTlsConfig(clientCertPem, clientKeyPem string) *tls.Config {
	cert, err := tls.X509KeyPair([]byte(clientCertPem), []byte(clientKeyPem))
	if err != nil {
		log.Fatalf("client: loadkeys: %s", err)
	}
	if len(cert.Certificate) != 2 {
		log.Fatal("client.crt should have 2 concatenated certificates: client + CA")
	}
	ca, err := x509.ParseCertificate(cert.Certificate[1])
	if err != nil {
		log.Fatal(err)
	}
	certPool := x509.NewCertPool()
	certPool.AddCert(ca)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      certPool,
	}
}

// dialerFor returns a function dialing service the way connect does: over TLS when the client
// certificate is configured, plain TCP otherwise.
func dialerFor(options Options) func(timeout time.Duration) (net.Conn, error) {
	var tlsConfig *tls.Config
	if len(options.ClientCertPem) > 0 && len(options.ClientKeyPem) > 0 {
		tlsConfig = getTlsConfig(strings.Replace(options.ClientCertPem, "\\n", "\n", -1),
			strings.Replace(options.ClientKeyPem, "\\n", "\n", -1))
	}
	return func(timeout time.Duration) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: timeout}
		if tlsConfig != nil {
			return tls.DialWithDialer(dialer, "tcp", options.Service, tlsConfig)
		}
		return dialer.Dial("tcp", options.Service)
	}
}

var client = RunnerClient{}

func connect(options Options) (err error) {
	var c *rpc.Client
	if !client.isConnected {
		dial := dialerFor(options)
		var conn net.Conn
		conn, err = dial(0)
		if err != nil {
			client.isConnected = false
			return err
		}
		c = rpc.NewClient(conn)

		client.c = c
		client.dial = dial
		//client.organizationID = options.OrganizationID
		client.userID = options.UserID
		client.token = options.Token
		client.currentDockerImage = options.DockerImage
		client.runnerRegion = options.Region
		client.cloudAccountID = options.CloudAccountID
		client.runnerMode = options.RunnerMode
		client.targetCloud = options.TargetCloud
	}

	return nil
}

var disconnectSignal = make(chan struct{})

type Options struct {
	Service string
	//OrganizationID        string
	UserID                string
	Token                 string
	ClientCertPem         string
	ClientKeyPem          string
	DockerImage           string
	Region                string
	CloudAccountID        string
	BlockTillFirstConnect bool
	RunnerMode            runner_enums.Mode
	TargetCloud           runner_enums.TargetCloud
}

func Connect(options Options, organizationID string) chan struct{} {
	firstTimeConnectSignal := make(chan struct{})
	if !client.isStarted {
		client.Lock()
		defer client.Unlock()
		if !client.isStarted {
			go func() {
				firstPing := true
				for {
					select {
					case <-disconnectSignal:
						client.isStarted = false
						client.isConnected = false
						return
					default:
						isConnectedOld := client.isConnected
						if !client.isConnected {
							connect(options)
						}
						if client.c != nil {
							err := client.Ping(firstPing, organizationID)
							if err != nil {
								if types.ErrInvalidUserKeySecret.Error() == err.Error() && options.RunnerMode == runner_enums.LOCAL {
									log.Fatal(err)
								}
								client.isConnected = false
								client.c.Close()
								client.c = nil
								if isConnectedOld != client.isConnected {
									//log only when connection status changes
								}
							} else {
								firstPing = false
								client.isConnected = true
								if isConnectedOld != client.isConnected {
									if options.BlockTillFirstConnect {
										<-firstTimeConnectSignal
										options.BlockTillFirstConnect = false
									}
								}
							}
						} else {
							client.isConnected = false
						}
						time.Sleep(5 * time.Second)
					}
				}
			}()
			client.isStarted = true
		}
	}
	return firstTimeConnectSignal
}

var ErrConnection = fmt.Errorf("client is not connected")

func Disconnect() error {
	if !client.isConnected {
		return ErrConnection
	}
	err := client.c.Close()
	if err != nil {
		return err
	}
	client.isConnected = false
	disconnectSignal <- struct{}{}
	return nil
}

func Get() *RunnerClient {
	return &client
}
