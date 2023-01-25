package main

import (
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
	//	create and connect RPC client
	client, err := rpc.Dial("tcp", service)
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
