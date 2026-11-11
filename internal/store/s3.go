package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/traumventures/offshore/internal/blobcache"
)

func newS3(ctx context.Context, g Grant, root string, ttl time.Duration) (Store, error) {
	if g.AccessKeyID == "" {
		return nil, errors.New("store: grant without credentials")
	}
	creds := credentials.NewStaticCredentialsProvider(g.AccessKeyID, g.SecretAccessKey, g.SessionToken)
	region := g.Region
	if region == "" {
		region = "auto"
	}
	client := s3.New(s3.Options{
		Region: region, Credentials: creds, UsePathStyle: g.PathStyle,
		BaseEndpoint: optional(g.Endpoint),
	})
	return blobcache.NewS3Store(client, g.Bucket, root, ttl), nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// stsAPI is the subset of the STS client used for grants.
type stsAPI interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput, opts ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

type s3Granter struct {
	c   Config
	sts stsAPI
}

func newS3Granter(ctx context.Context, c Config) (*s3Granter, error) {
	g := &s3Granter{c: c}
	if c.RoleARN == "" {
		if c.AccessKeyID == "" || c.SecretAccessKey == "" {
			return nil, fmt.Errorf("store %s: role_arn or access keys required (workers get scoped credentials)", c.Name)
		}
		return g, nil
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}
	if c.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, "")))
	} else if c.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	g.sts = sts.NewFromConfig(cfg, func(o *sts.Options) {
		if c.Endpoint != "" && !strings.Contains(c.Endpoint, "amazonaws.com") {
			o.BaseEndpoint = aws.String(c.Endpoint) // MinIO serves STS on its S3 endpoint
		}
	})
	return g, nil
}

func (g *s3Granter) Name() string { return g.c.Name }

// sessionPolicy limits assumed role credentials to the scopes.
func sessionPolicy(bucket, prefix string, scopes []string) string {
	var objects, prefixes []string
	for _, s := range scopes {
		p := strings.Trim(prefix+"/"+s, "/")
		objects = append(objects, "arn:aws:s3:::"+bucket+"/"+p+"/*")
		prefixes = append(prefixes, p+"/*")
	}
	pol := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Action": []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"}, "Resource": objects},
			{"Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::" + bucket,
				"Condition": map[string]any{"StringLike": map[string]any{"s3:prefix": prefixes}}},
		},
	}
	b, _ := json.Marshal(pol)
	return string(b)
}

func (g *s3Granter) Grant(ctx context.Context, scopes []string, ttl time.Duration) (Grant, error) {
	scopes = cleanScopes(scopes)
	out := Grant{Store: g.c.Name, Endpoint: g.c.Endpoint, Bucket: g.c.Bucket, Region: g.c.Region,
		Prefix: strings.Trim(g.c.Prefix, "/"), Scopes: scopes, PathStyle: g.c.PathStyle}
	if g.sts == nil {
		out.AccessKeyID, out.SecretAccessKey = g.c.AccessKeyID, g.c.SecretAccessKey
		return out, nil
	}
	secs := int32(min(max(ttl, 15*time.Minute), 12*time.Hour) / time.Second)
	res, err := g.sts.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: aws.String(g.c.RoleARN), RoleSessionName: aws.String("offshore-worker"),
		DurationSeconds: aws.Int32(secs), Policy: aws.String(sessionPolicy(g.c.Bucket, out.Prefix, scopes)),
	})
	if err != nil {
		return Grant{}, fmt.Errorf("store %s: assume role: %w", g.c.Name, err)
	}
	c := res.Credentials
	out.AccessKeyID, out.SecretAccessKey, out.SessionToken = aws.ToString(c.AccessKeyId), aws.ToString(c.SecretAccessKey), aws.ToString(c.SessionToken)
	if c.Expiration != nil {
		out.Expires = c.Expiration.Unix()
	}
	return out, nil
}
