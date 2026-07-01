package broker

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type PresignedUpload struct {
	URL    string
	Header http.Header
}

type ObjectMetadata struct {
	ContentLength int64
	ContentType   string
	ETag          string
}

type ObjectStore interface {
	PresignPutObject(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (PresignedUpload, error)
	HeadObject(ctx context.Context, key string) (ObjectMetadata, error)
}

type B2Store struct {
	bucket    string
	client    *s3.Client
	presigner *s3.PresignClient
}

func NewB2Store(ctx context.Context, cfg Config) (*B2Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.B2Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.B2AccessKeyID, cfg.B2SecretAccessKey, "")),
	)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(cfg.B2Endpoint)
		options.UsePathStyle = true
	})
	return &B2Store{
		bucket:    cfg.B2Bucket,
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func (s *B2Store) PresignPutObject(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (PresignedUpload, error) {
	response, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	}, func(options *s3.PresignOptions) {
		options.Expires = ttl
	})
	if err != nil {
		return PresignedUpload{}, err
	}
	return PresignedUpload{
		URL:    response.URL,
		Header: response.SignedHeader,
	}, nil
}

func (s *B2Store) HeadObject(ctx context.Context, key string) (ObjectMetadata, error) {
	response, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return ObjectMetadata{}, err
	}
	metadata := ObjectMetadata{
		ContentLength: aws.ToInt64(response.ContentLength),
		ContentType:   aws.ToString(response.ContentType),
		ETag:          strings.Trim(aws.ToString(response.ETag), `"`),
	}
	return metadata, nil
}

func PublicURL(baseURL, objectKey string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	segments := strings.Split(strings.Trim(objectKey, "/"), "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return baseURL + "/" + strings.Join(segments, "/")
}

func ShareURL(baseURL, objectKey string) string {
	return PublicURL(baseURL, "s/"+strings.Trim(objectKey, "/"))
}
