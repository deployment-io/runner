module github.com/deployment-io/deployment-runner

go 1.22.2

require (
	github.com/ankit-arora/go-utils v0.0.0-20230703175629-90641e500c7c
	github.com/ankit-arora/ipnets v0.0.0-20230525113803-5d737bfe484b
	github.com/ankit-arora/markdown v0.0.0-20240801070227-28eff2aa6649
	github.com/ankit-arora/nixpacks-go v0.0.0-20240925063829-e4f7ef9412db
	github.com/aws/aws-sdk-go-v2 v1.30.3
	github.com/aws/aws-sdk-go-v2/config v1.27.11
	github.com/aws/aws-sdk-go-v2/service/acm v1.25.4
	github.com/aws/aws-sdk-go-v2/service/cloudfront v1.36.0
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.38.6
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.35.1
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.156.0
	github.com/aws/aws-sdk-go-v2/service/ecr v1.27.4
	github.com/aws/aws-sdk-go-v2/service/ecs v1.41.7
	github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2 v1.30.5
	github.com/aws/aws-sdk-go-v2/service/iam v1.32.0
	github.com/aws/aws-sdk-go-v2/service/rds v1.81.4
	github.com/aws/aws-sdk-go-v2/service/s3 v1.53.1
	github.com/aws/aws-sdk-go-v2/service/secretsmanager v1.31.1
	github.com/aws/aws-sdk-go-v2/service/servicediscovery v1.31.1
	github.com/aws/aws-sdk-go-v2/service/sts v1.28.6
	github.com/aws/smithy-go v1.20.3
	github.com/deployment-io/deployment-runner-kit v0.0.0-20240923144746-74d40cea67a9
	github.com/docker/docker v27.3.0+incompatible
	github.com/joho/godotenv v1.5.1
	github.com/moby/moby v27.3.0+incompatible
	go.mongodb.org/mongo-driver v1.14.0
	gopkg.in/src-d/go-git.v4 v4.13.1
)

require (
	github.com/AdaLogics/go-fuzz-headers v0.0.0-20230811130428-ced1acdcaa24 // indirect
	github.com/Microsoft/go-winio v0.6.1 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.2 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.17.11 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.15 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.15 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.0 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.3.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/autoscaling v1.40.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.11.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.3.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.11.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.17.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.20.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.23.4 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-connections v0.5.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.1 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/karrick/godirwalk v1.17.0 // indirect
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/klauspost/compress v1.17.7 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-runewidth v0.0.9 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/patternmatcher v0.6.0 // indirect
	github.com/moby/sys/sequential v0.5.0 // indirect
	github.com/moby/sys/user v0.1.0 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/moby/term v0.5.0 // indirect
	github.com/morikuni/aec v1.0.0 // indirect
	github.com/olekukonko/tablewriter v0.0.5 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rogpeppe/go-internal v1.10.0 // indirect
	github.com/sergi/go-diff v1.3.1 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/src-d/gcfg v1.4.0 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.49.0 // indirect
	go.opentelemetry.io/otel v1.24.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.19.0 // indirect
	go.opentelemetry.io/otel/metric v1.24.0 // indirect
	go.opentelemetry.io/otel/sdk v1.19.0 // indirect
	go.opentelemetry.io/otel/trace v1.24.0 // indirect
	golang.org/x/crypto v0.23.0 // indirect
	golang.org/x/mod v0.8.0 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sync v0.7.0 // indirect
	golang.org/x/sys v0.20.0 // indirect
	golang.org/x/time v0.5.0 // indirect
	golang.org/x/tools v0.6.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230731190214-cbb8c96f2d6d // indirect
	google.golang.org/grpc v1.58.3 // indirect
	google.golang.org/protobuf v1.34.1 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/src-d/go-billy.v4 v4.3.2 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gotest.tools/v3 v3.4.0 // indirect
)
