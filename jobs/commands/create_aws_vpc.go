package commands

import (
	"context"
	"fmt"
	"github.com/ankit-arora/ipnets"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/deployment-io/deployment-runner-kit/enums/parameters_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/region_enums"
	"github.com/deployment-io/deployment-runner-kit/enums/vpc_enums"
	"github.com/deployment-io/deployment-runner-kit/jobs"
	"github.com/deployment-io/deployment-runner-kit/vpcs"
	"net"
	"strconv"
	"time"
)

const upsertVpcKey = "upsertVpcs"

type CreateAwsVPC struct {
}

func getDefaultVpcName(parameters map[parameters_enums.Key]interface{}) (string, error) {
	//vpc-<organizationId>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("vpc-%s", organizationID), nil
}

func getDefaultInternetGatewayName(parameters map[parameters_enums.Key]interface{}) (string, error) {
	//ig-<organizationId>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ig-%s", organizationID), nil
}

func getDefaultElasticIPName(parameters map[parameters_enums.Key]interface{}) (string, error) {
	//elastic-ip-<organizationId>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("elastic-ip-%s", organizationID), nil
}

func getDefaultNatGatewayName(parameters map[parameters_enums.Key]interface{}) (string, error) {
	//nat-gateway-<organizationId>
	organizationID, err := jobs.GetParameterValue[string](parameters, parameters_enums.OrganizationID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("nat-gateway-%s", organizationID), nil
}

func getDefaultRouteTableName(parameters map[parameters_enums.Key]interface{}, isPrivate bool, availabilityZone string) (string, error) {
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

func getDefaultSubnetName(parameters map[parameters_enums.Key]interface{}, isPrivate bool, availabilityZone string) (string, error) {
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

func getSubnetData(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client, cidr string) ([]subnetInfo, error) {
	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return nil, err
	}

	availabilityZones, err := getAvailabilityZonesFromRegion(ec2Client, region.String())
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

func getPrivateCidrBlocks() []string {
	return []string{
		"10.0.0.0/8",
		"192.168.0.0/16",
		"172.16.0.0/12",
	}
}

func getEC2Client(parameters map[parameters_enums.Key]interface{}) (*ec2.Client, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return nil, err
	}

	ec2Client := ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		o.Region = region.String()
	})
	return ec2Client, nil
}

func createVpcIfNeeded(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client, cidrBlock string) (string, error) {
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

		vpcId := aws.ToString(createVpcOutput.Vpc.VpcId)

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
	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}
	upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
		Type:   vpc_enums.AwsVpc,
		Region: region,
		VpcId:  vpcId,
	})

	return vpcId, nil
}

func createAndAttachInternetGatewayIfNeeded(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client, vpcId string) (string, error) {
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
	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}
	upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
		Type:              vpc_enums.AwsVpc,
		Region:            region,
		InternetGatewayId: aws.ToString(internetGatewayId),
	})

	return aws.ToString(internetGatewayId), nil

}

func createSubnetIfNeeded(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client, subnet subnetInfo, vpcId string) (subnetId string, shouldSync bool, err error) {
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

	var subnetID string
	if len(describeSubnetsOutput.Subnets) > 0 {
		subnetID = aws.ToString(describeSubnetsOutput.Subnets[0].SubnetId)
	} else {
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

		subnetID = aws.ToString(createSubnetOutput.Subnet.SubnetId)

	}

	return subnetID, true, nil
}

func allocatePublicElasticIPIfNeeded(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client) (string, error) {
	elasticIPIdFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.ElasticIPAllocationID)
	if err == nil && len(elasticIPIdFromParams) > 0 {
		return elasticIPIdFromParams, nil
	}

	defaultElasticIPName, err := getDefaultElasticIPName(parameters)
	if err != nil {
		return "", err
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
		return "", err
	}

	var allocationID *string
	if len(allocationIpsOutput.Addresses) > 0 {
		allocationID = allocationIpsOutput.Addresses[0].AllocationId
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
			return "", err
		}

		allocationID = allocateAddressOutput.AllocationId
	}

	//sync allocation ID to server
	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}
	upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
		Type:                  vpc_enums.AwsVpc,
		Region:                region,
		ElasticIPAllocationId: aws.ToString(allocationID),
	})

	return aws.ToString(allocationID), nil
}

func createNatGatewayIfNeeded(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client, subnetID, allocationID string) (string, error) {
	natGatewayIdFromParams, err := jobs.GetParameterValue[string](parameters, parameters_enums.NatGatewayID)
	if err == nil && len(natGatewayIdFromParams) > 0 {
		return natGatewayIdFromParams, nil
	}

	defaultNatGatewayName, err := getDefaultNatGatewayName(parameters)
	if err != nil {
		return "", err
	}

	natGatewaysOutput, err := ec2Client.DescribeNatGateways(context.TODO(), &ec2.DescribeNatGatewaysInput{
		DryRun: aws.Bool(false),
		Filter: []ec2Types.Filter{
			{
				Name: aws.String("tag:Name"),
				Values: []string{
					defaultNatGatewayName,
				},
			},
		},
	})

	if err != nil {
		return "", err
	}

	var natGatewayId string
	if len(natGatewaysOutput.NatGateways) > 0 {
		natGatewayId = aws.ToString(natGatewaysOutput.NatGateways[0].NatGatewayId)
	} else {
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
			return "", err
		}

		natGatewayId = aws.ToString(createNatGatewayOutput.NatGateway.NatGatewayId)
	}

	//sync nat gateway
	region, err := jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
	if err != nil {
		return "", err
	}
	upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
		Type:         vpc_enums.AwsVpc,
		Region:       region,
		NatGatewayId: natGatewayId,
	})

	return natGatewayId, nil
}

func createRouteTableIfNeeded(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client, vpcId string, isPrivate bool, availabilityZone string) (routeTableName, routeTableId string, shouldSync bool, err error) {
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

func associateRouteTableToSubnetIfNeeded(parameters map[parameters_enums.Key]interface{}, ec2Client *ec2.Client, subnetId,
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

type routeTableInfo struct {
	routeTableId string
	isPrivate    bool
}

func (c *CreateAwsVPC) Run(parameters map[parameters_enums.Key]interface{}, logger jobs.Logger) (newParameters map[parameters_enums.Key]interface{}, err error) {
	//try creating subnets for each of these cidr blocks
	cidrBlocks := getPrivateCidrBlocks()
	ec2Client, err := getEC2Client(parameters)
	if err != nil {
		return parameters, err
	}
	for _, cidrBlock := range cidrBlocks {
		var vpcId string
		vpcId, err = createVpcIfNeeded(parameters, ec2Client, cidrBlock)
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

		var natGatewayId string
		var routeTables []routeTableInfo
		firstPublicSubnet := true
		shouldSyncSubnetsAll := false
		shouldSyncRouteTablesAll := false
		var subnetsDto []vpcs.SubnetDtoV1
		var routeTablesDto []vpcs.RouteTableDtoV1
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
					var allocationID string
					allocationID, err = allocatePublicElasticIPIfNeeded(parameters, ec2Client)
					if err != nil {
						return parameters, err
					}

					natGatewayId, err = createNatGatewayIfNeeded(parameters, ec2Client, subnetId, allocationID)
					if err != nil {
						return parameters, err
					}
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
					}
					routeTables = append(routeTables, routeTable)

					routeTableDtoV1 := vpcs.RouteTableDtoV1{
						Name:      routeTableName,
						ID:        publicRouteTableId,
						IsPrivate: vpcs.False,
					}

					routeTablesDto = append(routeTablesDto, routeTableDtoV1)

					firstPublicSubnet = false
				}

				//associate public subnet to public route table
				err = associateRouteTableToSubnetIfNeeded(parameters, ec2Client, subnetId, publicRouteTableId)
				if err != nil {
					return parameters, err
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
					IsPrivate: vpcs.True,
				}

				routeTablesDto = append(routeTablesDto, routeTableDtoV1)
			}

			isPrivate := vpcs.False
			if subnet.isPrivate {
				isPrivate = vpcs.True
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
		var region region_enums.Type
		region, err = jobs.GetParameterValue[region_enums.Type](parameters, parameters_enums.Region)
		if err != nil {
			return parameters, err
		}

		if shouldSyncSubnetsAll {
			upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
				Type:    vpc_enums.AwsVpc,
				Region:  region,
				Subnets: subnetsDto,
			})
		}

		if shouldSyncRouteTablesAll {
			upsertVpcsPipeline.Add(upsertVpcKey, vpcs.UpsertVpcDtoV1{
				Type:        vpc_enums.AwsVpc,
				Region:      region,
				RouteTables: routeTablesDto,
			})
		}

		for _, routeTable := range routeTables {
			if !routeTable.isPrivate {
				//create internet gateway route for public route table
				err = createRouteIfNeeded(ec2Client, routeTable.routeTableId, internetGatewayId, "")
				if err != nil {
					return parameters, err
				}
			} else {
				//create nat gateway route for private route table
				err = createRouteIfNeeded(ec2Client, routeTable.routeTableId, "", natGatewayId)
				if err != nil {
					return parameters, err
				}
			}
		}

		return parameters, nil
	}

	return parameters, fmt.Errorf("error creating vpc for any cidr block")
}
