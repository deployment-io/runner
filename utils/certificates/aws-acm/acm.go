package aws_acm

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/acm"
)

type Manager struct {
	acmClient *acm.Client
	region    string
}

func NewManager(region string) (*Manager, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	acmClient := acm.NewFromConfig(cfg)

	return &Manager{
		acmClient: acmClient,
		region:    region,
	}, nil
}

func (m *Manager) RequestCertificate() error {

	return nil
}
