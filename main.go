package main

import (
	"github.com/deployment-io/deployment-runner/jobs/commands"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"github.com/deployment-io/jobs-runner-kit/jobs/types"
	"net/rpc"
	"time"
)

var service = "localhost:1234"

//var service = "nlb-deployment-load-balancer-8240e82289b3f92e.elb.eu-west-1.amazonaws.com:443"

func initClient() (*rpc.Client, error) {
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

	return rpc.Dial("tcp", service)
}

func main() {
	for true {
		client, err := initClient()
		if err != nil {
			time.Sleep(1 * time.Minute)
			continue
		}

		args := types.Args{A: "", P: "password"}

		var jobsDto types.JobsDtoV1
		err = client.Call("Jobs.GetPendingV1", args, &jobsDto)
		if err != nil {
			time.Sleep(1 * time.Minute)
			continue
		}

		for _, jobDtoV1 := range jobsDto.Jobs {
			parameters := jobDtoV1.Parameters
			logger, err := loggers.Get(parameters)
			if err != nil {
				//TODO send message back
			}
			for _, commandEnum := range jobDtoV1.CommandEnums {
				command, err := commands.Get(commandEnum)
				if err != nil {
					//TODO send message back
					continue
				}
				parameters, err = command.Run(parameters, logger)
				if err != nil {
					//TODO send message back
					continue
				}
			}
		}

		time.Sleep(15 * time.Second)
	}
}
