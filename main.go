package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/s3manager"
)

type Options struct {
	ACL     string
	Verbose bool
}

func main() {
	opts := Options{}
	flag.StringVar(&opts.ACL, "acl", "", "set acl")
	flag.BoolVar(&opts.Verbose, "v", false, "verbose")
	flag.Parse()

	cfg, err := external.LoadDefaultAWSConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to load SDK config, %s", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// Set the AWS Region that the service clients should use
	// cfg.Region = endpoints
	uploader := s3manager.NewUploader(cfg)

	cp := func() error {
		src, err := filepath.Abs(flag.Arg(1))
		if err != nil {
			return fmt.Errorf("parse source: %w", err)
		}

		dst, err := url.Parse(flag.Arg(2))
		if err != nil {
			return fmt.Errorf("parse destination: %w", err)
		}

		if dst.Scheme != "s3" {
			return fmt.Errorf("only s3 scheme is supported as destination")
		}

		return filepath.Walk(src, func(path string, info os.FileInfo, _ error) error {
			if info == nil || info.IsDir() {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open source file: %w", err)
			}
			defer file.Close()

			key := strings.TrimSuffix(dst.Path, "/") + strings.TrimPrefix(path, src)

			if opts.Verbose {
				fmt.Printf("copying %s => %s\n", path, key)
			}

			_, err = uploader.UploadWithContext(ctx, &s3manager.UploadInput{
				ACL:    s3.ObjectCannedACL(opts.ACL),
				Bucket: aws.String(dst.Hostname()),
				Key:    aws.String(key),
				Body:   file,
			})
			if err != nil {
				return fmt.Errorf("upload: %w", err)
			}
			return nil
		})
	}

	switch flag.Arg(0) {
	case "cp":
		err = cp()
	default:
		err = fmt.Errorf("invalid command. must be 'cp'")
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
