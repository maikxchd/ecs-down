package downscaler

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
)

type DownScaler struct {
	*Config
	asg *autoscaling.AutoScaling
	ec2 *ec2.EC2
	ecs *ecs.ECS
}

type Config struct {
	Service          string
	Cluster          string
	ASG              string
	DesiredCount     int64
	BatchSize        int
	InstanceType     string
	Region           string
	InstanceFlip     bool
	SortByAge        bool
	TaskCountDetect  bool
	AllowASGMismatch bool

	AgentVersionThreshold string
}

func New(config *Config) *DownScaler {
	awsConfig := &aws.Config{
		Region: &config.Region,
	}
	awsSession := session.Must(session.NewSession(awsConfig))

	return &DownScaler{
		Config: config,
		asg:    autoscaling.New(awsSession),
		ec2:    ec2.New(awsSession),
		ecs:    ecs.New(awsSession),
	}
}

func (d *DownScaler) Run() error {
	ctx := context.Background()

	containerInstances, err := d.findDrainableContainerInstances(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Found %d drainable container instances.\n", len(containerInstances))

	s, err := d.ecsService(ctx)
	if err != nil {
		return err
	}

	originalTaskCount := *s.DesiredCount
	maxToRemove := originalTaskCount - d.Config.DesiredCount
	if maxToRemove == 0 {
		return fmt.Errorf("Though we had %d drainable instances, no room to decrease ECS cluster size. aborting.", len(containerInstances))
	}

	for start := 0; start < len(containerInstances) && start < int(maxToRemove); start += d.BatchSize {
		end := start + d.BatchSize
		if l := len(containerInstances); end > l {
			end = l
		}
		s, err = d.ScaleDown(ctx, s, containerInstances[start:end])
		if err != nil {
			return err
		}
	}

	fmt.Println(strings.Repeat("*", 80))

	if d.Config.InstanceFlip {
		log.Printf("Returning ECS back to original task count %d", originalTaskCount)
		_, err = d.updateECSService(ctx, originalTaskCount)
		if err != nil {
			log.Println("Success!")
		}
		return err
	}
	// Set the ASG's final min, max, and desired count.
	return d.updateASG(ctx, d.DesiredCount, true)

}

func (d *DownScaler) ScaleDown(ctx context.Context, service *ecs.Service, containerInstances []*string) (*ecs.Service, error) {
	desiredCount := *service.DesiredCount - int64(len(containerInstances))
	instanceDesired := desiredCount

	if !d.Config.InstanceFlip {
		// Figure out instance desired count
		asg, err := d.describeASG(ctx)
		if err != nil {
			return nil, err
		}
		asgDesired := aws.Int64Value(asg.DesiredCapacity)
		if instanceDesired > asgDesired {
			instanceDesired = asgDesired - int64(len(containerInstances))
			mismatch := fmt.Sprintf("mismatched container and instance count %d != %d", *service.DesiredCount, asgDesired)
			if !d.Config.AllowASGMismatch {
				return nil, fmt.Errorf("%s not allowed; use -allow-mismatch to allow", mismatch)
			}
			log.Printf("Warning: %s. but mismatch mode enabled; will reduce instances to %d", mismatch, instanceDesired)
		}

	}

	fmt.Println(strings.Repeat("*", 80))

	// Drain container instances.
	log.Println("Draining container instances:")
	for _, ci := range containerInstances {
		fmt.Printf("\t%s\n", *ci)
	}
	drained, err := d.drainContainerInstances(ctx, containerInstances)
	if err != nil {
		return nil, err
	}

	if desiredCount > 0 {
		// Scale down ECS tasks.
		log.Printf("Scaling down ECS task count to %d...", desiredCount)
		service, err = d.updateECSService(ctx, desiredCount)
		if err != nil {
			return nil, err
		}

		if !d.Config.InstanceFlip {
			log.Printf("Scaling down ASG instance count to %d...\n", instanceDesired)
			if err := d.updateASG(ctx, instanceDesired, false); err != nil {
				return nil, err
			}
		}
	}

	// Terminate drained instances.
	log.Println("Terminating container instances:")
	for _, ci := range drained {
		fmt.Printf("\t%s\n", *ci.Ec2InstanceId)
	}
	if err := d.terminateContainerInstances(ctx, drained); err != nil {
		return nil, err
	}

	return service, nil
}
