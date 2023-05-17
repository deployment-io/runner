package aws_s3

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func NewClient(s3Region string, cfg aws.Config) (*s3.Client, error) {
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.Region = s3Region
	})

	return s3Client, nil
}
