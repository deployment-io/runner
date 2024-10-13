package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/deployment-io/deployment-runner-kit/enums/runner_enums"
	"github.com/deployment-io/deployment-runner-kit/types"
	"log"
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
}

func getTlsClient(service, clientCertPem, clientKeyPem string) (*rpc.Client, error) {
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
	config := tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      certPool,
	}
	conn, err := tls.Dial("tcp", service, &config)
	if err != nil {
		return nil, err
	}

	//create and connect RPC client
	return rpc.NewClient(conn), nil
}

var client = RunnerClient{}

func connect(options Options) (err error) {
	var c *rpc.Client
	if !client.isConnected {
		if len(options.ClientCertPem) > 0 && len(options.ClientKeyPem) > 0 {
			options.ClientCertPem = strings.Replace(options.ClientCertPem, "\\n", "\n", -1)
			options.ClientKeyPem = strings.Replace(options.ClientKeyPem, "\\n", "\n", -1)
			c, err = getTlsClient(options.Service, options.ClientCertPem, options.ClientKeyPem)
		} else {
			c, err = rpc.Dial("tcp", options.Service)
		}

		if err != nil {
			client.isConnected = false
			return err
		}

		client.c = c
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
