package client

import (
	"fmt"
	"net/rpc"
	"sync"
	"time"
)

type RunnerClient struct {
	sync.Mutex
	c           *rpc.Client
	isConnected bool
	isStarted   bool
}

var client = RunnerClient{}

func connect(connectionURI string) error {
	if !client.isConnected {

		//cert, err := tls.LoadX509KeyPair("/Users/ankit/Developer/deployment/certs-test/client.crt", "/Users/ankit/Developer/deployment/certs-test/client1.key")
		//if err != nil {
		//	log.Fatalf("client: loadkeys: %s", err)
		//}
		//fmt.Println(len(cert.Certificate))
		//if len(cert.Certificate) != 2 {
		//	log.Fatal("client.crt should have 2 concatenated certificates: client + CA")
		//}
		//ca, err := x509.ParseCertificate(cert.Certificate[1])
		//if err != nil {
		//	log.Fatal(err)
		//}
		//certPool := x509.NewCertPool()
		//certPool.AddCert(ca)
		//config := tls.Config{
		//	Certificates: []tls.Certificate{cert},
		//	RootCAs:      certPool,
		//}
		//
		//conn, err := tls.Dial("tcp", service, &config)
		//if err != nil {
		//	log.Fatalf("client: dial: %s", err)
		//}
		//defer conn.Close()
		//log.Println("client: connected to: ", conn.RemoteAddr())
		//
		////	create and connect RPC client
		//client := rpc.NewClient(conn)

		//client, err := rpc.Dial("tcp", service)
		//if err != nil {
		//	log.Fatal("dialing:", err)
		//}

		c, err := rpc.Dial("tcp", connectionURI)
		if err != nil {
			client.isConnected = false
			return err
		}

		client.c = c
	}

	return nil
}

var disconnectSignal = make(chan struct{})

func Connect(connectionURI string, blockTillFirstConnect bool) chan struct{} {
	firstTimeConnectSignal := make(chan struct{})
	if !client.isStarted {
		client.Lock()
		defer client.Unlock()
		if !client.isStarted {
			go func() {
				for {
					select {
					case <-disconnectSignal:
						client.isStarted = false
						client.isConnected = false
						return
					default:
						isConnectedOld := client.isConnected
						if !client.isConnected {
							connect(connectionURI)
						}
						if client.c != nil {
							err := client.Ping()
							if err != nil {
								client.isConnected = false
								client.c.Close()
								client.c = nil
								if isConnectedOld != client.isConnected {
									//log only when connection status changes
								}
							} else {
								client.isConnected = true
								if isConnectedOld != client.isConnected {
									if blockTillFirstConnect {
										<-firstTimeConnectSignal
										blockTillFirstConnect = false
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
