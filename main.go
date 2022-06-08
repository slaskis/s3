package main

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Options struct {
	ACL     string
	Verbose bool
}

func main() {
	opts := Options{}
	flag.StringVar(&opts.ACL, "acl", "private", "set acl")
	flag.BoolVar(&opts.Verbose, "verbose", false, "verbose")
	flag.Parse()

	ctx := context.Background()

	// trap Ctrl+C and call cancel on the context
	ctx, cancel := context.WithCancel(ctx)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to load SDK config, %s", err)
		os.Exit(1)
	}

	cli := &Client{
		Config: cfg,
	}

	switch flag.Arg(0) {
	case "cp":
		err = cli.Cp(ctx, CpInput{
			Src:     flag.Arg(1),
			Dst:     flag.Arg(2),
			ACL:     types.ObjectCannedACL(opts.ACL),
			Verbose: opts.Verbose,
		})
	case "dl":
		err = cli.Dl(ctx, DlInput{
			Concurrency: 100,
			Src:         flag.Arg(1),
			Dst:         flag.Arg(2),
		})
	case "ls":
		err = cli.Ls(ctx, LsInput{
			Src: flag.Arg(1),
		})
	default:
		err = fmt.Errorf("invalid command. must be 'cp', 'dl' or 'ls'")
		flag.Usage()
	}

	if err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

type Client struct {
	Config aws.Config
}

type CpInput struct {
	Src     string
	Dst     string
	ACL     types.ObjectCannedACL
	Verbose bool
}

func (c *Client) clientForBucket(ctx context.Context, bucket string) (*s3.Client, error) {
	s3client := s3.NewFromConfig(c.Config)

	region, err := manager.GetBucketRegion(ctx, s3client, bucket)
	if err != nil {
		return nil, fmt.Errorf("get bucket region: %w", err)
	}

	if c.Config.Region != region {
		regionCfg := c.Config.Copy()
		regionCfg.Region = region
		s3client = s3.NewFromConfig(regionCfg)
	}

	return s3client, nil
}

func (c *Client) Cp(ctx context.Context, input CpInput) error {
	src, err := filepath.Abs(strings.TrimPrefix(input.Src, "file://"))
	if err != nil {
		return fmt.Errorf("parse source: %w", err)
	}

	dst, err := url.Parse(input.Dst)
	if err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}

	if dst.Scheme != "s3" {
		return fmt.Errorf("only s3 scheme is supported as destination")
	}

	s3Client, err := c.clientForBucket(ctx, dst.Hostname())
	if err != nil {
		return fmt.Errorf("client for bucket: %w", err)
	}
	uploader := manager.NewUploader(s3Client)

	return filepath.Walk(src, func(path string, info os.FileInfo, _ error) error {
		if info == nil {
			return nil
		} else if info.IsDir() {
			if info.Name() == ".git" {
				fmt.Fprintln(os.Stderr, "skipping .git directory")
				return filepath.SkipDir
			}
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open source file: %w", err)
		}
		defer file.Close()

		key := strings.TrimPrefix(strings.TrimSuffix(dst.Path, "/")+strings.TrimPrefix(path, src), "/")

		if input.Verbose {
			fmt.Printf("copying %s => %s (%s)\n", path, key, input.ACL)
		}

		_, err = uploader.Upload(ctx, &s3.PutObjectInput{
			ACL:         input.ACL,
			Bucket:      aws.String(dst.Hostname()),
			Key:         aws.String(key),
			ContentType: aws.String(mime.TypeByExtension(filepath.Ext(path))),
			Body:        file,
		})
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		return nil
	})
}

type LsInput struct {
	Src string
}

func (c *Client) Ls(ctx context.Context, input LsInput) error {
	if input.Src == "" {
		s3Client := s3.NewFromConfig(c.Config)
		res, err := s3Client.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err != nil {
			return fmt.Errorf("list buckets: %w", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
		fmt.Fprintln(w, "name\tcreated")
		for _, row := range res.Buckets {
			fmt.Fprintf(w, "%s\t%s\t\n",
				aws.ToString(row.Name),
				row.CreationDate.Format(time.RFC822))
		}
		return w.Flush()
	}

	src, err := url.Parse(input.Src)
	if err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}

	if src.Scheme != "s3" {
		return fmt.Errorf("only s3 scheme is supported as destination")
	}

	s3Client, err := c.clientForBucket(ctx, src.Hostname())
	if err != nil {
		return fmt.Errorf("client for bucket: %w", err)
	}

	res, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(src.Hostname()),
		Prefix: aws.String(strings.TrimPrefix(src.Path, "/")),
	})
	if err != nil {
		var operr s3.ResponseError
		if errors.As(err, &operr) {
			log.Println(operr)
			log.Printf("%# v", operr)
		}
		log.Printf("%# v", err)
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintln(w, "key\tetag\tlast modified\tsize\tstorage class")
	for _, row := range res.Contents {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
			aws.ToString(row.Key),
			aws.ToString(row.ETag),
			row.LastModified.Format(time.RFC822),
			row.Size,
			row.StorageClass)
	}
	return w.Flush()
}

type DlInput struct {
	Concurrency int
	Src         string
	Dst         string
}

// dl downloads files as defined in an s3 inventory manifest
// ex. `s3 dl s3://sample-bucket/inventory/2020-08-10T00-00Z/manifest.json file://.`
func (c *Client) Dl(ctx context.Context, input DlInput) error {
	src, err := url.Parse(flag.Arg(1))
	if err != nil {
		return fmt.Errorf("parse source: %w", err)
	}
	if src.Scheme != "s3" {
		return fmt.Errorf("only s3 scheme is supported as manifest source")
	}

	s3Client, err := c.clientForBucket(ctx, src.Hostname())
	if err != nil {
		return fmt.Errorf("client for bucket: %w", err)
	}
	downloader := manager.NewDownloader(s3Client)

	dst, err := filepath.Abs(strings.TrimPrefix(flag.Arg(2), "file://"))
	if err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}

	res, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(src.Hostname()),
		Key:    aws.String(strings.TrimPrefix(src.Path, "/")),
	})
	if err != nil {
		return fmt.Errorf("unable to download manifest: %w", err)
	}

	manifest := struct {
		SourceBucket      string `json:"sourceBucket"`
		DestinationBucket string `json:"destinationBucket"`
		Version           string `json:"version"`
		CreationTimestamp string `json:"creationTimestamp"`
		FileFormat        string `json:"fileFormat"`
		FileSchema        string `json:"fileSchema"`
		Files             []struct {
			Key         string `json:"key"`
			Size        int    `json:"size"`
			MD5Checksum string `json:"MD5checksum"`
		} `json:"files"`
	}{}
	err = json.NewDecoder(res.Body).Decode(&manifest)
	if err != nil {
		return fmt.Errorf("unable to parse manifest: %w", err)
	}

	if manifest.FileFormat != "CSV" {
		return fmt.Errorf("only csv format supported")
	}
	if manifest.FileSchema != "Bucket, Key, Size, LastModifiedDate, ETag, StorageClass, ObjectLockRetainUntilDate, ObjectLockMode, ObjectLockLegalHoldStatus" {
		return fmt.Errorf("unexpected file schema")
	}

	type Row struct {
		Bucket                    string
		Key                       string
		Size                      int64
		LastModifiedDate          time.Time
		ETag                      string
		StorageClass              string
		ObjectLockRetainUntilDate string
		ObjectLockMode            string
		ObjectLockLegalHoldStatus string
	}

	var errs sync.Map
	var ok, skip, total uint32
	var done sync.WaitGroup
	rows := make(chan Row, input.Concurrency*10)

	errSkipped := fmt.Errorf("skipped")

	download := func(row Row) error {
		path := filepath.Join(dst, row.Key)
		err := os.MkdirAll(filepath.Dir(path), 0o777)
		if err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}

		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}
		defer file.Close()

		stat, err := file.Stat()
		if err != nil && os.IsNotExist(err) {
			// nvm, download
		} else if err != nil {
			return fmt.Errorf("stat: %w", err)
		} else if stat.ModTime().Equal(row.LastModifiedDate) && stat.Size() == row.Size {
			return errSkipped
		}

		_, err = downloader.Download(ctx, file, &s3.GetObjectInput{
			Bucket: aws.String(row.Bucket),
			Key:    aws.String(row.Key),
		})
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}

		err = os.Chtimes(file.Name(), row.LastModifiedDate, row.LastModifiedDate)
		if err != nil {
			return fmt.Errorf("chtimes: %w", err)
		}

		return nil
	}

	worker := func() {
		for row := range rows {
			atomic.AddUint32(&total, 1)

			err := download(row)
			if err == errSkipped {
				log.Println("skipped", row.Key)
				atomic.AddUint32(&skip, 1)
			} else if err != nil {
				log.Println("failed", err)
				errs.Store(row.Key, err)
				break
			} else {
				log.Println("downloaded", row.Key)
				atomic.AddUint32(&ok, 1)
			}
		}
	}

	for i := 0; i < input.Concurrency; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			worker()
		}()
	}

	started := time.Now()
	defer func() {
		fmt.Printf("completed %d/%d/%d downloads with errors in %s:\n", ok, skip, total, time.Since(started))
		errs.Range(func(k, v interface{}) bool {
			fmt.Printf("\t%s:%s\n", k, v)
			return true
		})
	}()

	for _, file := range manifest.Files {
		log.Println("get manifest data", manifest.SourceBucket, file.Key)
		res, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(manifest.SourceBucket),
			Key:    aws.String(file.Key),
		})
		if err != nil {
			return fmt.Errorf("unable to download inventory data: %w", err)
		}

		body := res.Body
		defer body.Close()

		if aws.ToString(res.ContentType) == "application/x-gzip" {
			log.Println("is gzipped")
			body, err = gzip.NewReader(body)
			if err != nil {
				return fmt.Errorf("unzip: %w", err)
			}
			defer body.Close()
		}

		c := csv.NewReader(body)
		c.LazyQuotes = true
		c.ReuseRecord = true
		c.FieldsPerRecord = len(strings.Split(manifest.FileSchema, ", "))
		c.TrimLeadingSpace = true

		for {
			if e := ctx.Err(); e != nil {
				return fmt.Errorf("ctx: %w", e)
			}

			row, err := c.Read()
			if err == io.EOF {
				log.Println("csv eof")
				break

			} else if err != nil {
				return fmt.Errorf("csv read: %w", err)
			}

			size, err := strconv.Atoi(row[2])
			if err != nil {
				return fmt.Errorf("parse time: %w", err)
			}

			modifiedAt, err := time.Parse(time.RFC3339, row[3])
			if err != nil {
				return fmt.Errorf("parse time: %w", err)
			}

			select {
			case <-ctx.Done():
			case rows <- Row{
				Bucket:           row[0],
				Key:              row[1],
				Size:             int64(size),
				LastModifiedDate: modifiedAt,
				ETag:             row[4],
				// ...
			}:
			}
		}
	}

	// no more rows will be added
	log.Println("closing rows")
	close(rows)

	log.Println("waiting for downloads")
	done.Wait()

	return nil
}
