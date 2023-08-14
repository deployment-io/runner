package commands

import (
	"context"
	"fmt"
	"github.com/ankit-arora/ipnets"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/vpc_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/types"
	"github.com/deployment-io/deployment-runner-kit/vpcs"
	"github.com/deployment-io/deployment-runner/utils/loggers"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const upsertVpcKey = "upsertVpcs"

type CreateDefaultAwsVPC struct {
}

func getDefaultVpcName(parameters map[string]interface{}) (string, error) {
	//vpc-<organizationId>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("vpc-%s", organizationID), nil
}

func getDefaultInternetGatewayName(parameters map[string]interface{}) (string, error) {
	//ig-<organizationId>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ig-%s", organizationID), nil
}

func getDefaultElasticIPName(parameters map[string]interface{}, az string) (string, error) {
	//elastic-ip-<organizationId>-public-eu-west-1b
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("elastic-ip-%s-%s", organizationID, az), nil
}

func getDefaultNatGatewayName(parameters map[string]interface{}, az string) (string, error) {
	//nat-gateway-<organizationId>-public-eu-west-1b
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("nat-gateway-%s-%s", organizationID, az), nil
}

func getDefaultRouteTableName(parameters map[string]interface{}, isPrivate bool, availabilityZone string) (string, error) {
	//route-table-<organizationId>-public-eu-west-1b
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	privateStr := "public"
	if isPrivate {
		privateStr = "private"
	}
	return fmt.Sprintf("route-table-%s-%s-%s", organizationID, privateStr, availabilityZone), nil
}

func getAvailabilityZonesFromRegion(ec2Client *ec2.Client, region string) ([]string, error) {
	describeAvailabilityZonesInput := &ec2.DescribeAvailabilityZonesInput{
		AllAvailabilityZones: aws.Bool(true),
		DryRun:               aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("region-name"),
				Values: []string{
					region,
				},
			},
		},
	}
	describeAvailabilityZonesOutput, err := ec2Client.DescribeAvailabilityZones(context.TODO(), describeAvailabilityZonesInput)
	if err != nil {
		return nil, err
	}
	var availabilityZones []string
	for _, az := range describeAvailabilityZonesOutput.AvailabilityZones {
		if aws.ToString(az.ZoneType) == "availability-zone" {
			availabilityZones = append(availabilityZones, aws.ToString(az.ZoneName))
		}
	}
	return availabilityZones, nil
}

func divideInToSubnets(cidr string, count int) ([]string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ipNets, err := ipnets.SubnetInto(network, count)
	if err != nil {
		return nil, err
	}
	var subnetCidrs []string
	for _, ipNet := range ipNets {
		subnetCidrs = append(subnetCidrs, ipNet.String())
	}
	return subnetCidrs, nil
}

func getDefaultSubnetName(parameters map[string]interface{}, isPrivate bool, availabilityZone string) (string, error) {
	//subnet-<organizationId>-public-eu-west-1b
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	privateStr := "public"
	if isPrivate {
		privateStr = "private"
	}
	return fmt.Sprintf("subnet-%s-%s-%s", organizationID, privateStr, availabilityZone), nil
}

type subnetInfo struct {
	name      string
	cidr      string
	az        string
	isPrivate bool
	subnetId  string
}

func getSubnetData(parameters map[string]interface{}, ec2Client *ec2.Client, cidr string) ([]subnetInfo, error) {
	region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
	if err != nil {
		return nil, err
	}

	availabilityZones, err := getAvailabilityZonesFromRegion(ec2Client, region_enums.Type(region).String())
	if err != nil {
		return nil, err
	}

	subnetCount := 2 * len(availabilityZones)
	subnetCidrs, err := divideInToSubnets(cidr, subnetCount)
	if err != nil {
		return nil, err
	}
	var subnetData []subnetInfo
	isPrivate := false
	cidrIndex := 0
	for i := 0; i < 2; i++ {
		for _, availabilityZone := range availabilityZones {
			subnetName, err := getDefaultSubnetName(parameters, isPrivate, availabilityZone)
			if err != nil {
				return nil, err
			}
			info := subnetInfo{
				name:      subnetName,
				cidr:      subnetCidrs[cidrIndex],
				az:        availabilityZone,
				isPrivate: isPrivate,
			}
			subnetData = append(subnetData, info)
			cidrIndex++
		}
		isPrivate = true
	}

	return subnetData, nil
}

// "10.0.0.0/8",
func getPrivateCidrBlocks() []string {
	return []string{
		"192.168.0.0/16",
		"172.16.0.0/12",
	}
}

func createVpcIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, cidrBlock string) (string, error) {
	vpcIdFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.VpcID)
	if err == nil && len(vpcIdFromParams) > 0 {
		return vpcIdFromParams, nil
	}

	defaultVpcName, err := getDefaultVpcName(parameters)
	if err != nil {
		return "", err
	}

	describeVpcsOutput, err := ec2Client.DescribeVpcs(context.TODO(), &ec2.DescribeVpcsInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					defaultVpcName,
				},
			},
		},
	})

	if err != nil {
		return "", err
	}

	var vpcId string
	if len(describeVpcsOutput.Vpcs) > 0 {
		vpcId = aws.ToString(describeVpcsOutput.Vpcs[0].VpcId)
	} else {
		//create VPC
		createVpcInput := &ec2.CreateVpcInput{
			AmazonProvidedIpv6CidrBlock: aws.Bool(false),
			CidrBlock:                   aws.String(cidrBlock),
			DryRun:                      aws.Bool(false),
			InstanceTenancy:             ec2Types.TenancyDefault,
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeVpc,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
					{
						Key:   aws.String("Name"),
						Value: aws.String(defaultVpcName),
					},
				},
			}},
		}

		createVpcOutput, err := ec2Client.CreateVpc(context.TODO(), createVpcInput)
		if err != nil {
			return "", err
		}

		vpcId = aws.ToString(createVpcOutput.Vpc.VpcId)

		newVpcAvailableWaiter := ec2.NewVpcAvailableWaiter(ec2Client)
		err = newVpcAvailableWaiter.Wait(context.TODO(), &ec2.DescribeVpcsInput{
			DryRun: aws.Bool(false),
			VpcIds: []string{vpcId},
		}, 10*time.Minute)
		if err != nil {
			return "", err
		}
	}

	//sync VPC ID to server
	region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}
	upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
		Type:   vpc_enums.AwsVpc,
		Region: region_enums.Type(region),
		VpcId:  vpcId,
	})

	return vpcId, nil
}

func createAndAttachInternetGatewayIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, vpcId string) (string, error) {
	internetGatewayIdFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.InternetGatewayID)
	if err == nil && len(internetGatewayIdFromParams) > 0 {
		return internetGatewayIdFromParams, nil
	}

	defaultInternetGatewayName, err := getDefaultInternetGatewayName(parameters)
	if err != nil {
		return "", err
	}

	internetGatewaysOutput, err := ec2Client.DescribeInternetGateways(context.TODO(), &ec2.DescribeInternetGatewaysInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					defaultInternetGatewayName,
				},
			},
		},
	})

	if err != nil {
		return "", err
	}

	var internetGatewayId *string
	if len(internetGatewaysOutput.InternetGateways) > 0 {
		internetGatewayId = internetGatewaysOutput.InternetGateways[0].InternetGatewayId
	} else {
		//create internet gateway
		createInternetGatewayInput := &ec2.CreateInternetGatewayInput{
			DryRun: aws.Bool(false),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeInternetGateway,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(defaultInternetGatewayName),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
				},
			}},
		}

		var createInternetGatewayOutput *ec2.CreateInternetGatewayOutput

		createInternetGatewayOutput, err = ec2Client.CreateInternetGateway(context.TODO(), createInternetGatewayInput)
		if err != nil {
			return "", err
		}

		internetGatewayId = createInternetGatewayOutput.InternetGateway.InternetGatewayId

		//attach internet gateway to vpc
		attachInternetGatewayInput := &ec2.AttachInternetGatewayInput{
			InternetGatewayId: internetGatewayId,
			VpcId:             aws.String(vpcId),
			DryRun:            aws.Bool(false),
		}

		_, err = ec2Client.AttachInternetGateway(context.TODO(), attachInternetGatewayInput)
		if err != nil {
			return "", err
		}
	}

	//sync IG ID to server
	region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}
	upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
		Type:              vpc_enums.AwsVpc,
		Region:            region_enums.Type(region),
		InternetGatewayId: aws.ToString(internetGatewayId),
	})

	return aws.ToString(internetGatewayId), nil

}

func createSubnetIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, subnet subnetInfo, vpcId string) (subnetId string, shouldSync bool, err error) {
	subnetsFromParams, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.Subnets)
	if err == nil && len(subnetsFromParams) > 0 {
		subnetIdFromParams, ok := subnetsFromParams[subnet.name]
		if ok && len(subnetIdFromParams) > 0 {
			return subnetIdFromParams, false, nil
		}
	}

	describeSubnetsOutput, err := ec2Client.DescribeSubnets(context.TODO(), &ec2.DescribeSubnetsInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					subnet.name,
				},
			},
		},
	})
	if err != nil {
		return "", false, err
	}

	for _, s := range describeSubnetsOutput.Subnets {
		if s.State == ec2Types.SubnetStateAvailable {
			subnetId = aws.ToString(describeSubnetsOutput.Subnets[0].SubnetId)
			break
		}
	}

	if len(subnetId) == 0 {
		createSubnetInput := &ec2.CreateSubnetInput{
			VpcId:            aws.String(vpcId),
			AvailabilityZone: aws.String(subnet.az),
			CidrBlock:        aws.String(subnet.cidr),
			DryRun:           aws.Bool(false),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeSubnet,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(subnet.name),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
					{
						Key:   aws.String("availability zone"),
						Value: aws.String(subnet.az),
					},
					{
						Key:   aws.String("is private"),
						Value: aws.String(strconv.FormatBool(subnet.isPrivate)),
					},
				},
			}},
		}

		createSubnetOutput, err := ec2Client.CreateSubnet(context.TODO(), createSubnetInput)
		if err != nil {
			return "", false, err
		}

		subnetId = aws.ToString(createSubnetOutput.Subnet.SubnetId)

		subnetAvailableWaiter := ec2.NewSubnetAvailableWaiter(ec2Client)
		err = subnetAvailableWaiter.Wait(context.TODO(), &ec2.DescribeSubnetsInput{
			SubnetIds: []string{
				subnetId,
			},
		}, 10*time.Minute)
		if err != nil {
			return "", false, err
		}
	}

	return subnetId, true, nil
}

func allocatePublicElasticIPIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, az string) (allocationID, allocationName string, err error) {
	defaultElasticIPName, err := getDefaultElasticIPName(parameters, az)
	if err != nil {
		return "", "", err
	}

	elasticIPsFromParams, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.ElasticIPAllocations)
	if err == nil && len(elasticIPsFromParams) > 0 {
		elasticIPIdFromParams, ok := elasticIPsFromParams[defaultElasticIPName]
		if ok && len(elasticIPIdFromParams) > 0 {
			return elasticIPIdFromParams, defaultElasticIPName, nil
		}
	}

	allocationIpsOutput, err := ec2Client.DescribeAddresses(context.TODO(), &ec2.DescribeAddressesInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					defaultElasticIPName,
				},
			},
		},
	})

	if err != nil {
		return "", "", err
	}

	if len(allocationIpsOutput.Addresses) > 0 {
		allocationID = aws.ToString(allocationIpsOutput.Addresses[0].AllocationId)
	} else {
		allocateAddressInput := &ec2.AllocateAddressInput{
			DryRun: aws.Bool(false),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeElasticIp,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(defaultElasticIPName),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
				},
			}},
		}

		allocateAddressOutput, err := ec2Client.AllocateAddress(context.TODO(), allocateAddressInput)
		if err != nil {
			return "", "", err
		}

		allocationID = aws.ToString(allocateAddressOutput.AllocationId)
	}

	return allocationID, defaultElasticIPName, nil
}

func createNatGatewayIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, subnetID, allocationID, az string) (natGatewayId string, natGatewayName string, shouldWaitTillAvailable bool, err error) {

	defaultNatGatewayName, err := getDefaultNatGatewayName(parameters, az)
	if err != nil {
		return "", "", false, err
	}

	natGatewaysFromParams, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.NatGateways)
	if err == nil && len(natGatewaysFromParams) > 0 {
		natGatewayIdFromParams, ok := natGatewaysFromParams[defaultNatGatewayName]
		if ok && len(natGatewayIdFromParams) > 0 {
			return natGatewayIdFromParams, defaultNatGatewayName, false, nil
		}
	}

	describeNatGatewaysInput := &ec2.DescribeNatGatewaysInput{
		DryRun: aws.Bool(false),
		Filter: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					defaultNatGatewayName,
				},
			},
		},
	}
	natGatewaysOutput, err := ec2Client.DescribeNatGateways(context.TODO(), describeNatGatewaysInput)

	if err != nil {
		return "", "", false, err
	}

	for _, natGateway := range natGatewaysOutput.NatGateways {
		if natGateway.State == ec2Types.NatGatewayStateAvailable {
			natGatewayId = aws.ToString(natGateway.NatGatewayId)
			shouldWaitTillAvailable = false
			break
		}
	}

	if len(natGatewayId) == 0 {
		createNatGatewayInput := &ec2.CreateNatGatewayInput{
			SubnetId:         aws.String(subnetID),
			AllocationId:     aws.String(allocationID),
			ClientToken:      aws.String(defaultNatGatewayName), //unique token
			ConnectivityType: ec2Types.ConnectivityTypePublic,
			DryRun:           aws.Bool(false),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeNatgateway,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(defaultNatGatewayName),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
				},
			}},
		}
		createNatGatewayOutput, err := ec2Client.CreateNatGateway(context.TODO(), createNatGatewayInput)

		if err != nil {
			return "", "", false, err
		}

		natGatewayId = aws.ToString(createNatGatewayOutput.NatGateway.NatGatewayId)

		shouldWaitTillAvailable = true
	}

	return natGatewayId, defaultNatGatewayName, shouldWaitTillAvailable, nil
}

func waitForNatGatewayAvailability(ec2Client *ec2.Client, natGatewayId string) error {
	describeNatGatewaysInput := &ec2.DescribeNatGatewaysInput{
		DryRun: aws.Bool(false),
		NatGatewayIds: []string{
			natGatewayId,
		},
	}

	waiter := ec2.NewNatGatewayAvailableWaiter(ec2Client)
	err := waiter.Wait(context.TODO(), describeNatGatewaysInput, time.Minute*10)

	if err != nil {
		return err
	}

	return nil
}

func createRouteTableIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, vpcId string, isPrivate bool, availabilityZone string) (routeTableName, routeTableId string, shouldSync bool, err error) {
	routeTableName, err = getDefaultRouteTableName(parameters, isPrivate, availabilityZone)
	if err != nil {
		return "", "", false, err
	}
	routeTablesFromParams, err := jobs.GetParameterValue[map[string]string](parameters, parameters_enums.RouteTables)
	if err == nil && len(routeTablesFromParams) > 0 {
		routeTableIdFromParams, ok := routeTablesFromParams[routeTableName]
		if ok && len(routeTableIdFromParams) > 0 {
			return routeTableName, routeTableIdFromParams, false, nil
		}
	}

	describeSubnetsOutput, err := ec2Client.DescribeRouteTables(context.TODO(), &ec2.DescribeRouteTablesInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					routeTableName,
				},
			},
		},
	})
	if err != nil {
		return "", "", false, err
	}

	if len(describeSubnetsOutput.RouteTables) > 0 {
		routeTableId = aws.ToString(describeSubnetsOutput.RouteTables[0].RouteTableId)
	} else {
		createRouteTableInput := &ec2.CreateRouteTableInput{
			VpcId:  aws.String(vpcId),
			DryRun: aws.Bool(false),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeRouteTable,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(routeTableName),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
					{
						Key:   aws.String("availability zone"),
						Value: aws.String(availabilityZone),
					},
					{
						Key:   aws.String("is private"),
						Value: aws.String(strconv.FormatBool(isPrivate)),
					},
				},
			}},
		}
		createRouteTableOutput, err := ec2Client.CreateRouteTable(context.TODO(), createRouteTableInput)
		if err != nil {
			return "", "", false, err
		}
		routeTableId = aws.ToString(createRouteTableOutput.RouteTable.RouteTableId)

	}

	return routeTableName, routeTableId, true, nil
}

func associateRouteTableToSubnetIfNeeded(parameters map[string]interface{}, ec2Client *ec2.Client, subnetId,
	routeTableId string) error {

	describeRouteTableOutput, err := ec2Client.DescribeRouteTables(context.TODO(), &ec2.DescribeRouteTablesInput{
		DryRun: aws.Bool(false),
		RouteTableIds: []string{
			routeTableId,
		},
	})
	if err != nil {
		return err
	}

	if len(describeRouteTableOutput.RouteTables) == 0 {
		return fmt.Errorf("error describing route table while associating: %s", routeTableId)
	}

	for _, a := range describeRouteTableOutput.RouteTables[0].Associations {
		if subnetId == aws.ToString(a.SubnetId) {
			//already associated to subnet
			return nil
		}
	}

	associateRouteTableInput := &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(routeTableId),
		DryRun:       aws.Bool(false),
		SubnetId:     aws.String(subnetId),
	}
	_, err = ec2Client.AssociateRouteTable(context.TODO(), associateRouteTableInput)

	if err != nil {
		return err
	}

	return nil
}

func createRouteIfNeeded(ec2Client *ec2.Client, routeTableId, internetGatewayId, natGatewayId string) error {
	describeRouteTableOutput, err := ec2Client.DescribeRouteTables(context.TODO(), &ec2.DescribeRouteTablesInput{
		DryRun: aws.Bool(false),
		RouteTableIds: []string{
			routeTableId,
		},
	})
	if err != nil {
		return err
	}

	if len(describeRouteTableOutput.RouteTables) == 0 {
		return fmt.Errorf("error describing route table while creating route: %s", routeTableId)
	}

	for _, r := range describeRouteTableOutput.RouteTables[0].Routes {
		if len(internetGatewayId) > 0 && internetGatewayId == aws.ToString(r.GatewayId) {
			return nil
		}
		if len(natGatewayId) > 0 && natGatewayId == aws.ToString(r.NatGatewayId) {
			return nil
		}
	}

	createRouteInput := &ec2.CreateRouteInput{
		RouteTableId:         aws.String(routeTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		DryRun:               aws.Bool(false),
	}

	if len(internetGatewayId) > 0 {
		createRouteInput.GatewayId = aws.String(internetGatewayId)
	}

	if len(natGatewayId) > 0 {
		createRouteInput.NatGatewayId = aws.String(natGatewayId)
	}
	_, err = ec2Client.CreateRoute(context.TODO(), createRouteInput)
	if err != nil {
		return err
	}
	return nil
}

func getDefaultSecurityGroupIdForVpc(parameters map[string]interface{}, ec2Client *ec2.Client, vpcId string) (securityGroupId string, err error) {
	securityGroupsOutput, err := ec2Client.DescribeSecurityGroups(context.TODO(), &ec2.DescribeSecurityGroupsInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("vpc-id"),
				Values: []string{
					vpcId,
				},
			},
			{
				Name: aws.String("group-name"),
				Values: []string{
					"default",
				},
			},
		},
	})

	if err != nil {
		return "", err
	}

	for _, sg := range securityGroupsOutput.SecurityGroups {
		if aws.ToString(sg.VpcId) == vpcId {
			securityGroupId = aws.ToString(sg.GroupId)
		}
	}

	if len(securityGroupId) == 0 {
		err = fmt.Errorf("no default security group found")
	}

	return
}

func getDefaultVpcSecurityGroupIngressRuleNameForPort(parameters map[string]interface{}) (string, error) {
	//sgin-<port>
	port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sgin-%d", port), nil
}

func addIngressRuleToDefaultVpcSecurityGroup(parameters map[string]interface{}, ec2Client *ec2.Client, vpcId, vpcCidr string) error {
	//TODO create new security group
	defaultSecurityGroupId, err := getDefaultSecurityGroupIdForVpc(parameters, ec2Client, vpcId)
	if err != nil {
		return err
	}
	defaultVpcSecurityGroupIngressRuleNameForPort, err := getDefaultVpcSecurityGroupIngressRuleNameForPort(parameters)
	if err != nil {
		return err
	}

	describeSecurityGroupRulesOutput, err := ec2Client.DescribeSecurityGroupRules(context.TODO(), &ec2.DescribeSecurityGroupRulesInput{
		DryRun: aws.Bool(false),
		Filters: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					defaultVpcSecurityGroupIngressRuleNameForPort,
				},
			},
			{
				Name: aws.String("group-id"),
				Values: []string{
					defaultSecurityGroupId,
				},
			},
		},
	})

	if err != nil {
		return err
	}

	if len(describeSecurityGroupRulesOutput.SecurityGroupRules) == 0 {
		port, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Port)
		if err != nil {
			return err
		}
		authorizeSecurityGroupIngressInput := &ec2.AuthorizeSecurityGroupIngressInput{
			CidrIp:     aws.String(vpcCidr),
			DryRun:     aws.Bool(false),
			FromPort:   aws.Int32(int32(port)),
			GroupId:    aws.String(defaultSecurityGroupId),
			IpProtocol: aws.String("tcp"),
			TagSpecifications: []ec2Types.TagSpecification{{
				ResourceType: ec2Types.ResourceTypeSecurityGroupRule,
				Tags: []ec2Types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(defaultVpcSecurityGroupIngressRuleNameForPort),
					},
					{
						Key:   aws.String("created by"),
						Value: aws.String("deployment.io"),
					},
					{
						Key:   aws.String("vpc-default-security-group-id"),
						Value: aws.String(defaultSecurityGroupId),
					},
				},
			}},
			ToPort: aws.Int32(int32(port)),
		}
		_, err = ec2Client.AuthorizeSecurityGroupIngress(context.TODO(), authorizeSecurityGroupIngressInput)
		if err != nil {
			return err
		}

		describeSecurityGroupRulesOutput, err = ec2Client.DescribeSecurityGroupRules(context.TODO(), &ec2.DescribeSecurityGroupRulesInput{
			DryRun: aws.Bool(false),
			Filters: []ec2Types.Filter{
				{
					Name: aws.String("group-id"),
					Values: []string{
						defaultSecurityGroupId,
					},
				},
			},
		})

		if err != nil {
			return err
		}

		var ingressRules []vpcs.DefaultSecurityIngressRuleDtoV1
		for _, securityGroupRule := range describeSecurityGroupRulesOutput.SecurityGroupRules {
			if !*securityGroupRule.IsEgress {
				ingressRules = append(ingressRules, vpcs.DefaultSecurityIngressRuleDtoV1{
					ID: aws.ToString(securityGroupRule.SecurityGroupRuleId),
				})
			}
		}

		region, err := jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
		if err != nil {
			return err
		}

		upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
			Type:                        vpc_enums.AwsVpc,
			Region:                      region_enums.Type(region),
			DefaultSecurityIngressRules: ingressRules,
			DefaultSecurityGroupId:      defaultSecurityGroupId,
		})
	}
	return nil
}

type routeTableInfo struct {
	routeTableId string
	isPrivate    bool
	az           string
}

func (c *CreateDefaultAwsVPC) Run(parameters map[string]interface{}, logger jobs.Logger) (newParameters map[string]interface{}, err error) {
	logsWriter, err := loggers.GetBuildLogsWriter(parameters, logger)
	if err != nil {
		return parameters, err
	}
	defer logsWriter.Close()
	defer func() {
		if err != nil {
			markBuildDone(parameters, err, logsWriter)
		}
	}()
	//try creating subnets for each of these cidr blocks
	cidrBlocks := getPrivateCidrBlocks()
	ec2Client, err := getEC2Client(parameters)
	if err != nil {
		return parameters, err
	}
	cidrBlockFromParams, _ := jobs.GetParameterValue[string](parameters, parameters_enums.VpcCidr)
	var masterNatID string
	for _, cidrBlock := range cidrBlocks {
		if len(cidrBlockFromParams) > 0 && cidrBlockFromParams != cidrBlock {
			continue
		}
		var vpcId string
		vpcId, err = createVpcIfNeeded(parameters, ec2Client, cidrBlock)
		if err != nil {
			return parameters, err
		}

		io.WriteString(logsWriter, fmt.Sprintf("Created VPC - %s\n", vpcId))

		err = addIngressRuleToDefaultVpcSecurityGroup(parameters, ec2Client, vpcId, cidrBlock)
		if err != nil {
			return parameters, err
		}
		var internetGatewayId string
		internetGatewayId, err = createAndAttachInternetGatewayIfNeeded(parameters, ec2Client, vpcId)
		if err != nil {
			return parameters, err
		}

		var publicRouteTableId string
		var subnetData []subnetInfo
		subnetData, err = getSubnetData(parameters, ec2Client, cidrBlock)
		if err != nil {
			return parameters, err
		}

		natGatewaysAzMap := make(map[string]string)
		var routeTables []routeTableInfo
		firstPublicSubnet := true
		shouldSyncSubnetsAll := false
		shouldSyncRouteTablesAll := false
		var subnetsDto []vpcs.SubnetDtoV1
		var routeTablesDto []vpcs.RouteTableDtoV1
		var natGatewaysDto []vpcs.NatGatewayDtoV1
		var natGatewayIdsToWait []string
		for _, subnet := range subnetData {
			//create subnet
			var subnetId string
			var shouldSyncSubnet bool
			subnetId, shouldSyncSubnet, err = createSubnetIfNeeded(parameters, ec2Client, subnet, vpcId)
			if err != nil {
				return parameters, err
			}
			if !shouldSyncSubnetsAll && shouldSyncSubnet {
				shouldSyncSubnetsAll = true
			}

			if !subnet.isPrivate {
				if firstPublicSubnet {
					//for first subnet which is public
					var shouldSyncRouteTable bool
					var routeTableName string
					routeTableName, publicRouteTableId, shouldSyncRouteTable, err = createRouteTableIfNeeded(parameters, ec2Client, vpcId, subnet.isPrivate, subnet.az)
					if err != nil {
						return parameters, err
					}
					if !shouldSyncRouteTablesAll && shouldSyncRouteTable {
						shouldSyncRouteTablesAll = true
					}

					routeTable := routeTableInfo{
						routeTableId: publicRouteTableId,
						isPrivate:    false,
						az:           subnet.az,
					}
					routeTables = append(routeTables, routeTable)

					routeTableDtoV1 := vpcs.RouteTableDtoV1{
						Name:      routeTableName,
						ID:        publicRouteTableId,
						IsPrivate: types.False,
					}

					routeTablesDto = append(routeTablesDto, routeTableDtoV1)

					firstPublicSubnet = false
				}

				//associate public subnet to public route table
				err = associateRouteTableToSubnetIfNeeded(parameters, ec2Client, subnetId, publicRouteTableId)
				if err != nil {
					return parameters, err
				}

				var shouldWaitForNatGatewayAvailability bool
				//get createMultipleNats bool from parameters - false by default
				createMultipleNats := false
				createMultipleNats, err = jobs.GetParameterValue[bool](parameters, parameters_enums.CreateMultipleNats)
				if err != nil {
					createMultipleNats = false
				}
				if createMultipleNats || len(natGatewaysDto) < 1 {
					var allocationId, allocationName string
					allocationId, allocationName, err = allocatePublicElasticIPIfNeeded(parameters, ec2Client, subnet.az)
					if err != nil {
						return parameters, err
					}
					var natGatewayId, natGatewayName string
					natGatewayId, natGatewayName, shouldWaitForNatGatewayAvailability, err = createNatGatewayIfNeeded(parameters, ec2Client, subnetId, allocationId, subnet.az)
					if err != nil {
						return parameters, err
					}
					if shouldWaitForNatGatewayAvailability {
						natGatewayIdsToWait = append(natGatewayIdsToWait, natGatewayId)
					}

					natGatewayDtoV1 := vpcs.NatGatewayDtoV1{
						Name:                    natGatewayName,
						ID:                      natGatewayId,
						ElasticIPAllocationName: allocationName,
						ElasticIPAllocationId:   allocationId,
					}
					natGatewaysDto = append(natGatewaysDto, natGatewayDtoV1)

					natGatewaysAzMap[subnet.az] = natGatewayId
					masterNatID = natGatewayId
				}
			} else {
				//create route table for each private subnet
				var routeTableId, routeTableName string
				var shouldSyncRouteTable bool
				routeTableName, routeTableId, shouldSyncRouteTable, err = createRouteTableIfNeeded(parameters, ec2Client, vpcId, subnet.isPrivate, subnet.az)
				if err != nil {
					return parameters, err
				}
				if !shouldSyncRouteTablesAll && shouldSyncRouteTable {
					shouldSyncRouteTablesAll = true
				}
				routeTable := routeTableInfo{
					routeTableId: routeTableId,
					isPrivate:    true,
					az:           subnet.az,
				}
				routeTables = append(routeTables, routeTable)

				//associate private subnet to route table
				err = associateRouteTableToSubnetIfNeeded(parameters, ec2Client, subnetId, routeTableId)
				if err != nil {
					return parameters, err
				}

				routeTableDtoV1 := vpcs.RouteTableDtoV1{
					Name:      routeTableName,
					ID:        routeTableId,
					IsPrivate: types.True,
				}

				routeTablesDto = append(routeTablesDto, routeTableDtoV1)
			}

			isPrivate := types.False
			if subnet.isPrivate {
				isPrivate = types.True
			}
			subnetDtoV1 := vpcs.SubnetDtoV1{
				Name:      subnet.name,
				ID:        subnetId,
				Cidr:      subnet.cidr,
				Az:        subnet.az,
				IsPrivate: isPrivate,
			}
			subnetsDto = append(subnetsDto, subnetDtoV1)
		}
		var region int64
		region, err = jobs.GetParameterValue[int64](parameters, parameters_enums.Region)
		if err != nil {
			return parameters, err
		}

		if shouldSyncSubnetsAll {
			upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
				Type:    vpc_enums.AwsVpc,
				Region:  region_enums.Type(region),
				Subnets: subnetsDto,
			})
		}

		if shouldSyncRouteTablesAll {
			upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
				Type:        vpc_enums.AwsVpc,
				Region:      region_enums.Type(region),
				RouteTables: routeTablesDto,
			})
		}

		upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
			Type:        vpc_enums.AwsVpc,
			Region:      region_enums.Type(region),
			NatGateways: natGatewaysDto,
		})

		upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
			Type:    vpc_enums.AwsVpc,
			Region:  region_enums.Type(region),
			VpcCidr: cidrBlock,
		})

		//wait for natGateway availability

		io.WriteString(logsWriter, fmt.Sprintf("Waiting for NAT Gateways to be available: %v\n", natGatewayIdsToWait))

		var wg sync.WaitGroup
		for _, nId := range natGatewayIdsToWait {
			wg.Add(1)
			go func(natGatewayId string) {
				defer wg.Done()
				errFromWaiting := waitForNatGatewayAvailability(ec2Client, natGatewayId)
				if errFromWaiting != nil {
					err = errFromWaiting
				}
			}(nId)
		}
		wg.Wait()
		if err != nil {
			return parameters, err
		}

		for _, routeTable := range routeTables {
			if !routeTable.isPrivate {
				//create internet gateway route for public route table
				err = createRouteIfNeeded(ec2Client, routeTable.routeTableId, internetGatewayId, "")
				if err != nil {
					return parameters, err
				}
			} else {
				natGatewayId, ok := natGatewaysAzMap[routeTable.az]
				if !ok {
					natGatewayId = masterNatID
				}
				//create nat gateway route for private route table
				err = createRouteIfNeeded(ec2Client, routeTable.routeTableId, "", natGatewayId)
				if err != nil {
					return parameters, err
				}
			}
		}

		jobs.SetParameterValue(parameters, parameters_enums.VpcID, vpcId)
		jobs.SetParameterValue(parameters, parameters_enums.VpcCidr, cidrBlock)
		jobs.SetParameterValue(parameters, parameters_enums.InternetGatewayID, internetGatewayId)

		if len(natGatewaysDto) > 0 {
			jobs.SetParameterValue[map[string]string](parameters, parameters_enums.NatGateways, vpcs.GetNatGatewaysMapFromDto(natGatewaysDto))
			jobs.SetParameterValue[map[string]string](parameters, parameters_enums.ElasticIPAllocations, vpcs.GetAllocationsMapFromDto(natGatewaysDto))
		}
		if len(subnetsDto) > 0 {
			jobs.SetParameterValue[map[string]string](parameters, parameters_enums.Subnets, vpcs.GetSubnetsMapFromDto(subnetsDto))
			jobs.SetParameterValue[primitive.A](parameters, parameters_enums.PrivateSubnets, vpcs.GetPrivateSubnetIdsFromDto(subnetsDto))
			jobs.SetParameterValue[primitive.A](parameters, parameters_enums.PublicSubnets, vpcs.GetPublicSubnetIdsFromDto(subnetsDto))
		}
		if len(routeTablesDto) > 0 {
			jobs.SetParameterValue(parameters, parameters_enums.RouteTables, vpcs.GetRouteTablesMapFromDto(routeTablesDto))
		}
		return parameters, nil
	}

	return parameters, fmt.Errorf("error creating vpc for any cidr block")
}
