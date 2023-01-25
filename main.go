package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/rpc"
)

var service = "localhost:1234"

type Args struct {
	A string
	P string
}

// JobDtoV1 represents a deployment job from the server
type JobDtoV1 struct {
	CloudType      string
	DeploymentType string
}

//JobsDtoV1 represents a list of jobs
type JobsDtoV1 struct {
	Count int
	Jobs  []JobDtoV1
}

func main() {

	cert, err := tls.LoadX509KeyPair("/Users/ankit/Developer/deployment/certs-test/client.crt", "/Users/ankit/Developer/deployment/certs-test/client1.key")
	if err != nil {
		log.Fatalf("client: loadkeys: %s", err)
	}
	fmt.Println(len(cert.Certificate))
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
		log.Fatalf("client: dial: %s", err)
	}
	defer conn.Close()
	log.Println("client: connected to: ", conn.RemoteAddr())

	//	create and connect RPC client
	client := rpc.NewClient(conn)
	if err != nil {
		log.Fatal("dialing:", err)
	}
	args := Args{"", "password"}

	var jobsDto JobsDtoV1
	err = client.Call("Jobs.GetPendingV1", args, &jobsDto)
	if err != nil {
		log.Fatal("arith error:", err)
	}
	fmt.Printf("a: %s, p: %s => count: %d, jobs: %v\n", args.A, args.P, jobsDto.Count, jobsDto.Jobs)
}
