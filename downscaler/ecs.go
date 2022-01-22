package downscaler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/pkg/errors"
)

// Returns the number of tasks that can be running on a container instance
// before it is eligible for draining.
//
// If a cluster runs 2 services (say, graphql and dnsmasq as a daemon), `drainAtTaskCount` returns 1
// because dnsmasq will still be running on the instance even after graphql has stopped.
//
// If a container instance is running 3 tasks (say, 2 graphql tasks and 1 dnsmasq daemon),
// `drainAtTaskCount` still returns 1 because the cluster is still running 2 _services_.
func (d *DownScaler) drainAtTaskCount(ctx context.Context) (int64, error) {
	out, err := d.ecs.DescribeClustersWithContext(ctx, &ecs.DescribeClustersInput{
		Clusters: []*string{&d.Cluster},
	})
	if err != nil {
		return -1, nil
	}

	if length := len(out.Clusters); length != 1 {
		return -1, fmt.Errorf("expected 1 cluster named %q, but found %d", d.Cluster, length)
	}

	count := out.Clusters[0].ActiveServicesCount
	if count == nil {
		return -1, fmt.Errorf("cluster %q	has \"nil\" active services count", d.Cluster)
	}

	return *count - 1, nil
}

func (d *DownScaler) ecsService(ctx context.Context) (*ecs.Service, error) {
	out, err := d.ecs.DescribeServicesWithContext(ctx, &ecs.DescribeServicesInput{
		Cluster:  &d.Cluster,
		Services: []*string{&d.Service},
	})
	if err != nil {
		return nil, err
	}

	if length := len(out.Services); length != 1 {
		return nil, fmt.Errorf("expected 1 service named %q, but found %d", d.Service, length)
	}

	return out.Services[0], nil
}

func (d *DownScaler) updateECSService(ctx context.Context, desiredCount int64) (*ecs.Service, error) {
	forceNewDeployment := false
	out, err := d.ecs.UpdateServiceWithContext(ctx, &ecs.UpdateServiceInput{
		Cluster:            &d.Cluster,
		Service:            &d.Service,
		ForceNewDeployment: &forceNewDeployment,
		DesiredCount:       &desiredCount,
	})
	if err != nil {
		return nil, err
	}

	err = d.ecs.WaitUntilServicesStableWithContext(ctx, &ecs.DescribeServicesInput{
		Cluster:  &d.Cluster,
		Services: []*string{&d.Service},
	})
	if err != nil {
		return nil, err
	}

	return out.Service, nil
}

// Returns a list of container instance ARNs, sorted by order of preference, for draining.
// https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_DeregisterContainerInstance.html
func (d *DownScaler) findDrainableContainerInstances(ctx context.Context) ([]*string, error) {
	var allArns []*string
	seen := make(map[string]bool)
	skipped := 0
	input := &ecs.ListContainerInstancesInput{
		Cluster: &d.Cluster,
	}

	findInstances := func(filter string) error {
		var arns []*string
		fn := func(page *ecs.ListContainerInstancesOutput, isLastPage bool) bool {
			for _, arnPtr := range page.ContainerInstanceArns {
				if !seen[*arnPtr] {
					seen[*arnPtr] = true
					arns = append(arns, arnPtr)
				} else {
					skipped += 1
				}
			}
			return page.NextToken != nil
		}

		skipped = 0
		initialArns := len(arns)
		if filter == "" {
			input.Filter = nil
		} else {
			input.Filter = aws.String(filter)
		}
		err := d.ecs.ListContainerInstancesPagesWithContext(ctx, input, fn)
		if err != nil {
			return err
		}
		fmt.Printf(" -> %s: Added %d instances (%d duplicates skipped) to candidates\n", filter, len(arns)-initialArns, skipped)

		if d.SortByAge && len(arns) > 1 {
			arns, err = d.sortECSContainersByInstanceAge(ctx, arns)
			if err != nil {
				return err
			}
		}
		allArns = append(allArns, arns...)
		return nil
	}

	// Container instances with old agent are first-pick
	if d.Config.AgentVersionThreshold != "" {
		query := "agentVersion < " + d.Config.AgentVersionThreshold
		fmt.Printf("Finding instances with %s\n", query)
		if err := findInstances(query); err != nil {
			return nil, err
		}
	}

	// Container instances of the matching type are next-pick for draining.
	instanceTypeFilter := ""
	if d.InstanceType != "" {
		instanceTypeFilter = "attribute:ecs.instance-type == " + d.InstanceType
		if err := findInstances(instanceTypeFilter); err != nil {
			return nil, err
		}
	}

	// Instances running few tasks are next.
	if d.Config.TaskCountDetect {
		// The number of tasks that can be running on a container instance before it is eligible for draining.
		runningCount, err := d.drainAtTaskCount(ctx)
		if err != nil {
			return nil, err
		}

		taskCountFilter := fmt.Sprintf("runningTasksCount <= %d", runningCount)
		if err := findInstances(taskCountFilter); err != nil {
			return nil, err
		}
	}

	// Anything leftover is last-pick.
	if err := findInstances(""); err != nil {
		return nil, err
	}

	// If there are c container instances and we want d, drain c - d container instances.
	drainCount := len(allArns) - int(d.DesiredCount)
	if drainCount <= 0 {
		return nil, fmt.Errorf("%d container instances are desired, but there are only %d currently running", d.DesiredCount, len(allArns))
	}

	return allArns[0:drainCount], nil
}

func (d *DownScaler) drainContainerInstances(ctx context.Context, containerInstanceARNs []*string) ([]*ecs.ContainerInstance, error) {
	draining := "DRAINING"
	input := &ecs.UpdateContainerInstancesStateInput{
		Cluster:            &d.Cluster,
		ContainerInstances: containerInstanceARNs,
		Status:             &draining,
	}
	out, err := d.ecs.UpdateContainerInstancesStateWithContext(ctx, input)
	if err != nil {
		return nil, err
	}

	return out.ContainerInstances, nil
}

func (d *DownScaler) sortECSContainersByInstanceAge(ctx context.Context, containerArns []*string) ([]*string, error) {
	containerArnToEc2ID := make(map[string]string)
	ec2IDToContainerArn := make(map[string]string)
	var ec2IDs []string

	// The API is limited to 100 instances, so run it as many times as needed to satisfy
	for _, containerArns := range paginateStringArray(aws.StringValueSlice(containerArns), 100) {
		info, err := d.ecs.DescribeContainerInstancesWithContext(ctx, &ecs.DescribeContainerInstancesInput{
			Cluster:            &d.Cluster,
			ContainerInstances: aws.StringSlice(containerArns),
		})
		if err != nil {
			return nil, errors.Wrap(err, "cannot describe container instances")
		}

		for _, instance := range info.ContainerInstances {
			ec2ID := aws.StringValue(instance.Ec2InstanceId)
			containerArn := aws.StringValue(instance.ContainerInstanceArn)
			containerArnToEc2ID[containerArn] = ec2ID
			ec2IDToContainerArn[ec2ID] = containerArn
			ec2IDs = append(ec2IDs, ec2ID)
		}
	}

	containerArnToInstanceAge := make(map[string]*time.Time)

	fn := func(page *ec2.DescribeInstancesOutput, hasNext bool) bool {
		for _, res := range page.Reservations {
			for _, instance := range res.Instances {
				containerArn := ec2IDToContainerArn[aws.StringValue(instance.InstanceId)]
				containerArnToInstanceAge[containerArn] = instance.LaunchTime
			}
		}
		return page.NextToken != nil
	}

	for _, instanceIDs := range paginateStringArray(ec2IDs, 200) {
		err := d.ec2.DescribeInstancesPagesWithContext(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: aws.StringSlice(instanceIDs),
		}, fn)
		if err != nil {
			return nil, errors.Wrap(err, "cannot describe instances")
		}
	}

	work := aws.StringValueSlice(containerArns)

	var err error
	sort.Slice(work, func(i, j int) bool {
		ti := containerArnToInstanceAge[work[i]]
		tj := containerArnToInstanceAge[work[j]]
		if ti == nil {
			err = fmt.Errorf("container ARN age empty for %s", work[i])
			log.Print(err)
			return false
		}
		if tj == nil {
			err = fmt.Errorf("container ARN age empty for %s", work[j])
			log.Print(err)
			return false
		}
		return tj.After(*ti)
	})
	return aws.StringSlice(work), err
}

func paginateStringArray(items []string, n int) [][]string {
	var slices [][]string

	for i := 0; i < len(items); i += n {
		endThreshold := i + n
		if endThreshold > len(items) {
			endThreshold = len(items)
		}
		slices = append(slices, items[i:endThreshold])
	}
	return slices
}
