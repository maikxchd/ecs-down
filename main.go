package main

import (
	"flag"
	"log"

	"github.com/maikxchd/ecs-down/downscaler"
)

var (
	// Required parameters.
	service      = flag.String("service", "", "The name of the ECS service to scale down.")
	cluster      = flag.String("cluster", "", "The name of ECS cluster that hosts the service.")
	asg          = flag.String("asg", "", "The name of the Auto Scaling Group to scale down.")
	desiredCount = flag.Int64("desired-count", 0, "The number of container instances the ECS cluster should run.")

	// Optional parameters.
	batchSize    = flag.Int("batch-size", 1, "The number of ECS tasks or container instances to terminate in each batch.")
	instanceType = flag.String("instance-type", "", `The container instance type that should be preferred for termination.
If not provided or if there are no instances of this type, all instances are eligible for termination.`)
	region           = flag.String("region", "us-west-2", "The AWS region containing the resources.")
	flipMode         = flag.Bool("instance-flip", false, "Flip instances instead of scaling down")
	sortAge          = flag.Bool("sort-age", false, "Sort instances in each group by instance age")
	disableTaskCount = flag.Bool("disable-task-count", false, "Disable task count detection")
	agentVersion     = flag.String("agent-version-before", "", "Prefer killing instances with agent version older than X (exclusive) e.g. '1.39.0'")
	mismatch         = flag.Bool("allow-mismatch", false, "Advanced: Allow mismatch between containers and instances.")
)

func main() {
	flag.Parse()
	//	log.SetFlags(0)

	if *service == "" {
		log.Fatal("Missing required argument: service")
	}
	if *cluster == "" {
		log.Fatal("Missing required argument: cluster")
	}
	if *asg == "" {
		log.Fatal("Missing required argument: asg")
	}
	if *desiredCount <= 0 {
		log.Fatal("desired-count must be a positive integer")
	}

	d := downscaler.New(&downscaler.Config{
		Service:      *service,
		Cluster:      *cluster,
		ASG:          *asg,
		DesiredCount: *desiredCount,
		BatchSize:    *batchSize,
		InstanceType: *instanceType,
		Region:       *region,

		InstanceFlip:          *flipMode,
		SortByAge:             *sortAge,
		AllowASGMismatch:      *mismatch,
		TaskCountDetect:       !*disableTaskCount,
		AgentVersionThreshold: *agentVersion,
	})
	if err := d.Run(); err != nil {
		log.Fatal(err)
	}
}
