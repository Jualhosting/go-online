package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Manager struct {
	client     *s3.Client
	bucketName string
	enabled    bool
}

func NewR2Manager() (*R2Manager, error) {
	endpoint := os.Getenv("R2_ENDPOINT")
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secretKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	bucketName := os.Getenv("R2_BUCKET_NAME")

	if endpoint == "" || accessKey == "" || secretKey == "" || bucketName == "" {
		log.Println("[R2] Missing Cloudflare R2 credentials. Serving statically via local disk fallback.")
		return &R2Manager{enabled: false}, nil
	}

	log.Printf("[R2] Cloudflare R2 configured. Bucket: %s | Endpoint: %s", bucketName, endpoint)

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		config.WithRegion("auto"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load R2 configuration: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	return &R2Manager{
		client:     client,
		bucketName: bucketName,
		enabled:    true,
	}, nil
}

func (r *R2Manager) IsEnabled() bool {
	return r.enabled
}

func (r *R2Manager) UploadFile(ctx context.Context, r2Key string, data []byte, contentType string) error {
	if !r.enabled {
		return fmt.Errorf("R2 manager is disabled")
	}

	var ctype *string
	if contentType != "" {
		ctype = aws.String(contentType)
	}

	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(r.bucketName),
		Key:         aws.String(r2Key),
		Body:        bytes.NewReader(data),
		ContentType: ctype,
	})
	return err
}

func (r *R2Manager) DownloadFile(ctx context.Context, r2Key string) (io.ReadCloser, string, error) {
	if !r.enabled {
		return nil, "", fmt.Errorf("R2 manager is disabled")
	}

	output, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucketName),
		Key:    aws.String(r2Key),
	})
	if err != nil {
		return nil, "", err
	}

	contentType := ""
	if output.ContentType != nil {
		contentType = *output.ContentType
	}

	return output.Body, contentType, nil
}
