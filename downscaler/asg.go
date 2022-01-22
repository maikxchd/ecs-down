package downscaler

import (
	"context"

	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/pkg/errors"
)

func (d *DownScaler) updateASG(ctx context.Context, minSize int64, shouldSetMaxSize bool) error {
	input := &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: &d.ASG,
		MinSize:              &minSize,
		DesiredCapacity:      &minSize,
	}

	if shouldSetMaxSize {
		input.MaxSize = &minSize
	}

	if _, err := d.asg.UpdateAutoScalingGroupWithContext(ctx, input); err != nil {
		return err
	}

	return d.asg.WaitUntilGroupInServiceWithContext(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{&d.ASG},
	})
}

func (d *DownScaler) terminateContainerInstances(ctx context.Context, containerInstances []*ecs.ContainerInstance) error {
	instanceIDs := make([]*string, 0, len(containerInstances))

	decrementDesiredCapacity := false
	for _, ci := range containerInstances {
		instanceIDs = append(instanceIDs, ci.Ec2InstanceId)

		input := &autoscaling.TerminateInstanceInAutoScalingGroupInput{
			InstanceId:                     ci.Ec2InstanceId,
			ShouldDecrementDesiredCapacity: &decrementDesiredCapacity,
		}
		_, err := d.asg.TerminateInstanceInAutoScalingGroupWithContext(ctx, input)
		if err != nil {
			return err
		}
	}

	err := d.ec2.WaitUntilInstanceTerminatedWithContext(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return err
	}

	return nil
}

func (d *DownScaler) describeASG(ctx context.Context) (*autoscaling.Group, error) {
	result, err := d.asg.DescribeAutoScalingGroupsWithContext(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{&d.ASG},
	})
	if err != nil {
		return nil, errors.Wrap(err, "cannot describe ASG")
	}
	for _, g := range result.AutoScalingGroups {
		return g, nil
	}
	return nil, errors.New("Could not find ASG?")
}
