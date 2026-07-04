package broker

import (
	"context"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type ObjectMetadata struct {
	ContentLength int64
	ContentType   string
	ETag          string
}

type ObjectStore interface {
	HeadObject(ctx context.Context, key string) (ObjectMetadata, error)
	DownloadObject(ctx context.Context, key string, writer io.Writer) error
	PutObject(ctx context.Context, key, contentType string, size int64, reader io.Reader) (ObjectMetadata, error)
	DeleteObject(ctx context.Context, key string) error
}

type B2Store struct {
	bucket string
	client *s3.Client
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
		bucket: cfg.B2Bucket,
		client: client,
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

func (s *B2Store) DownloadObject(ctx context.Context, key string, writer io.Writer) error {
	response, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(writer, response.Body)
	return err
}

func (s *B2Store) PutObject(ctx context.Context, key, contentType string, size int64, reader io.Reader) (ObjectMetadata, error) {
	response, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          reader,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return ObjectMetadata{}, err
	}
	return ObjectMetadata{
		ContentLength: size,
		ContentType:   contentType,
		ETag:          strings.Trim(aws.ToString(response.ETag), `"`),
	}, nil
}

func (s *B2Store) DeleteObject(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
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
