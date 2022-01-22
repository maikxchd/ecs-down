# ecs-down
A tool for orchestrating the scale down of ECS clusters.

The downscaling algorithm is as follows:
1. Find drainable container instances
2. Drain `-batch-size` container instances.
3. Scale down ECS tasks by `-batch-size`.
4. Scale down ASG *desired* instances by `-batch-size`.
5. Terminate the drained container instances.
6. Repeat steps 2-5 for each batch until the cluster size is at `-desired-count`.
7. Finally, reduce ASG maximum to the new desired count (unless `-instance-flip` is used)

## Installation
```
GO111MODULE=on go get github.com/maikxchd/ecs-down
```

## Usage

```
$ ecs-down -help

Usage of ecs-down:
  -asg string
      The name of the Auto Scaling Group to scale down.
  -service string
      The name of the ECS service to scale down.
  -cluster string
      The name of ECS cluster that hosts the service.
  -desired-count int
      The number of container instances the ECS cluster should run.
  -batch-size int
      The number of ECS tasks or container instances to terminate in each batch. (default 1)
  -instance-type string
      The container instance type that should be preferred for termination.
      If not provided or if there are no instances of this type, all instances are eligible for termination.
  -agent-version-before string
      Prefer killing instances with agent version older than X (exclusive)
  -instance-flip
      Flip instances instead of scaling down EC2
  -region string
      The AWS region containing the resources. (default "us-west-2")
  -disable-task-count
      Disable task count detection
  -sort-age
      Sort instances in each group by instance age
```

## Examples
Scale down `gql-production` to 210 instances in batches of 20. `c6gd.16xlarge` container instances are selected for termination first.
```
ecs-down -asg prod-gql -batch-size 20 -cluster gql-production -service gql-production -desired-count 210
```
Scale down `visage-prod` to 45 instances in batches of 5.
```
ecs-down -asg prod-visage -batch-size 5 -cluster visage-prod -service visage-prod -desired-count 45
```

## Instance Selection Priority

Instances are selected for termination in this priority:

1. If `agent-version-before` is set, these are top for termination
2. If `instance-type` is set, these are next priority termination
3. Any instances running less than some number of tasks are priority for termination (disable this group with `disable-task-count` flag)
4. All other instances fill the last group.

If `sort-age` is used, then each sub-group is sorted so that the oldest instances are first choice. Otherwise, there is no ordering guarantee, it is whatever the API chooses to do.

## Instance Flipping

In some situations it is not possible to get enough instances to say, double EC2 desired count or not plausible to get new instances rapidly and you want to repeatedley cycle out old instances in smaller quantities. For this purpose, `-instance-flip` option will go towards desired *ECS* but keep EC2 Autoscaling Group the same size (allowing ASG to replace instances that are killed) then increases ECS count again.

As an example, in May 2020 GraphQL needed to cycle all 180 instances. To do this we set desired in ECS and EC2 to 190 and then did a bunch of runs of:
```
 ecs-down -asg edge-platform-production-gql-production -cluster gql-production \
     -service gql-production -desired-count 181 -batch-size 3 -instance-flip \
     --agent-version-before "1.37.0"
 ```
 Each run of the program only cycled 9 hosts (in batches of 3) allowing the instance count to always remain above 180. When the instances replaced back to 190 (because with `-instance-flip` the tool never dropped the ASG desired) the program was run again until all agents were cycled.
