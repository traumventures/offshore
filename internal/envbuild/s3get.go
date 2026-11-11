package envbuild

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Get downloads a virtual-hosted S3 URL
// (https://<bucket>.s3.<region>.amazonaws.com/<key>) with the default AWS
// credential chain.
func s3Get(raw string) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	bucket, rest, ok := strings.Cut(u.Host, ".s3.")
	region, ok2 := strings.CutSuffix(rest, ".amazonaws.com")
	if !ok || !ok2 || bucket == "" || region == "" {
		return nil, fmt.Errorf("not an S3 URL: %s", raw)
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	out, err := s3.NewFromConfig(cfg).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(strings.TrimPrefix(u.Path, "/")),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}
