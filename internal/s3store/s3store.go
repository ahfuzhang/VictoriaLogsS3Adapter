// Package s3store wraps an S3-compatible object store (AWS S3, Aliyun OSS, MinIO, ...)
// and defines the object naming convention for VictoriaLogs partition backups.
//
// Backup object key convention:
//
//	<expanded -s3.prefix>/<YYYYMMDD>.tar
//
// where YYYYMMDD is the VictoriaLogs partition name (UTC day).
package s3store

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
)

// Environment variables holding S3 credentials.
const (
	EnvAccessKeyID     = "access_key_id"
	EnvSecretAccessKey = "secret_access_key"
)

// DefaultPartSize is the multipart part size used for streaming uploads.
// The maximum object size is DefaultPartSize * 10000 parts (640GiB).
const DefaultPartSize = 64 * 1024 * 1024

// Config holds S3 connection settings.
type Config struct {
	Endpoint string
	Region   string
	Bucket   string
	Prefix   string
}

// RegisterFlags registers -s3.endpoint, -s3.region, -s3.bucket and -s3.prefix on fs.
func RegisterFlags(fs *flag.FlagSet) *Config {
	c := &Config{}
	fs.StringVar(&c.Endpoint, "s3.endpoint", "", "S3 endpoint URL, e.g. https://oss-cn-hongkong.aliyuncs.com")
	fs.StringVar(&c.Region, "s3.region", "", "S3 region, e.g. cn-hongkong")
	fs.StringVar(&c.Bucket, "s3.bucket", "", "S3 bucket name")
	fs.StringVar(&c.Prefix, "s3.prefix", "", "S3 key prefix template. {VAR} is replaced with the environment variable VAR; "+
		"${FORMAT} is replaced in UTC with YYYY (year), MM (month), DD (day) of the backed up partition date and "+
		"HH (hour), MI (minute), SS (second) of the backup start time, each at most once, optionally separated by '-', '_' or '.', "+
		"e.g. ${YYYYMMDD_HHMISS} or ${DD-MM-YYYY_HH_MI}")
	return c
}

// Validate checks that required fields are set.
func (c *Config) Validate() error {
	if c.Endpoint == "" {
		return errors.New("-s3.endpoint must be set")
	}
	if c.Bucket == "" {
		return errors.New("-s3.bucket must be set")
	}
	return nil
}

// Client is an S3 client bound to a single bucket.
type Client struct {
	s3     *s3.Client
	bucket string
}

// ObjectInfo describes an object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// CredentialsFromEnv reads S3 credentials from the environment.
func CredentialsFromEnv() (accessKeyID, secretAccessKey string, err error) {
	accessKeyID = os.Getenv(EnvAccessKeyID)
	secretAccessKey = os.Getenv(EnvSecretAccessKey)
	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", fmt.Errorf("environment variables %q and %q must be set", EnvAccessKeyID, EnvSecretAccessKey)
	}
	return accessKeyID, secretAccessKey, nil
}

// New creates a client from cfg using credentials from the environment and
// verifies that the bucket is accessible.
func New(ctx context.Context, cfg *Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ak, sk, err := CredentialsFromEnv()
	if err != nil {
		return nil, err
	}
	return NewWithCredentials(ctx, cfg, ak, sk)
}

// NewWithCredentials creates a client with explicit credentials and verifies bucket access.
func NewWithCredentials(ctx context.Context, cfg *Config, accessKeyID, secretAccessKey string) (*Client, error) {
	endpoint := cfg.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("cannot parse -s3.endpoint=%q: %w", cfg.Endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme in -s3.endpoint=%q; must be http or https", cfg.Endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in -s3.endpoint=%q", cfg.Endpoint)
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	s3c := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(u.Scheme + "://" + u.Host),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		// Virtual-hosted style for AWS, GCS and Aliyun OSS endpoints, path style for everything else (e.g. MinIO).
		UsePathStyle: !virtualHostedHost(u.Hostname()),
		// Do not add CRC32 checksums / aws-chunked trailers unless required: many
		// S3-compatible stores (Aliyun OSS, MinIO, ...) do not support them.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           3,
	})
	c := &Client{s3: s3c, bucket: cfg.Bucket}
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func virtualHostedHost(host string) bool {
	for _, suffix := range []string{".amazonaws.com", ".amazonaws.com.cn", ".aliyuncs.com", ".googleapis.com"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// Bucket returns the bucket name.
func (c *Client) Bucket() string { return c.bucket }

// Check verifies that the bucket exists and is accessible with the configured credentials.
func (c *Client) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Use ListObjectsV2 instead of HeadBucket: HeadBucket requires the bucket-level
	// permission (e.g. oss:GetBucketInfo), which object-only credentials often lack.
	_, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(c.bucket), MaxKeys: aws.Int32(1)})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("S3 bucket %q does not exist", c.bucket)
		}
		return fmt.Errorf("cannot access S3 bucket %q: %w", c.bucket, err)
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Upload streams r into the object key using multipart upload with an unknown size.
// The object becomes visible only after the whole stream is uploaded successfully.
// It returns the number of uploaded bytes.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, contentType string) (int64, error) {
	up := manager.NewUploader(c.s3, func(u *manager.Uploader) {
		u.PartSize = DefaultPartSize
		u.Concurrency = 1 // bounds memory to ~PartSize
	})
	cr := &countingReader{r: r}
	in := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   cr,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := up.Upload(ctx, in); err != nil {
		return 0, fmt.Errorf("cannot upload s3://%s/%s: %w", c.bucket, key, err)
	}
	return cr.n.Load(), nil
}

// Open opens the object key for reading. The caller must close the returned reader.
func (c *Client) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, 0, fmt.Errorf("cannot open s3://%s/%s: %w", c.bucket, key, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

// Stat returns object info. found is false when the object does not exist.
func (c *Client) Stat(ctx context.Context, key string) (info ObjectInfo, found bool, err error) {
	st, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, false, nil
		}
		return ObjectInfo{}, false, fmt.Errorf("cannot stat s3://%s/%s: %w", c.bucket, key, err)
	}
	return ObjectInfo{Key: key, Size: aws.ToInt64(st.ContentLength), LastModified: aws.ToTime(st.LastModified)}, true, nil
}

// List returns all objects whose keys start with prefix (recursively).
func (c *Client) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	p := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot list s3://%s/%s: %w", c.bucket, prefix, err)
		}
		for _, obj := range page.Contents {
			out = append(out, ObjectInfo{Key: aws.ToString(obj.Key), Size: aws.ToInt64(obj.Size), LastModified: aws.ToTime(obj.LastModified)})
		}
	}
	return out, nil
}

// Delete removes the object key.
func (c *Client) Delete(ctx context.Context, key string) error {
	if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("cannot delete s3://%s/%s: %w", c.bucket, key, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var nf *types.NotFound
	var nk *types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nk) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchBucket":
			return true
		}
	}
	return false
}

// BackupFileName returns the backup object base name for the partition date (YYYYMMDD).
func BackupFileName(date string) string {
	return date + ".tar"
}

// BackupKey returns the object key for the given partition date (YYYYMMDD).
// The date fields (YYYY, MM, DD) of the prefix template's time placeholders are
// taken from the partition date and the time-of-day fields (HH, MI, SS) from now.
func BackupKey(tmpl *pathtmpl.Template, now time.Time, date string) (string, error) {
	day, err := time.Parse("20060102", date)
	if err != nil {
		return "", fmt.Errorf("invalid partition date %q; want YYYYMMDD: %w", date, err)
	}
	return pathtmpl.Join(tmpl.Expand(day, now), BackupFileName(date)), nil
}

// FindBackup looks up the backup object for the partition date (YYYYMMDD).
//
// If the prefix template has no time placeholder, the exact key is checked.
// Otherwise all objects under the static part of the prefix are listed and the
// most recently modified object whose key matches the whole template followed
// by /<date>.tar is returned.
func (c *Client) FindBackup(ctx context.Context, tmpl *pathtmpl.Template, date string) (ObjectInfo, bool, error) {
	name := BackupFileName(date)
	if !tmpl.HasTime() {
		return c.Stat(ctx, pathtmpl.Join(tmpl.Expand(time.Time{}, time.Time{}), name))
	}
	objs, err := c.List(ctx, tmpl.StaticPrefix())
	if err != nil {
		return ObjectInfo{}, false, err
	}
	re := tmpl.KeyRegexp(name)
	var best ObjectInfo
	found := false
	for _, o := range objs {
		if !re.MatchString(o.Key) {
			continue
		}
		if !found || o.LastModified.After(best.LastModified) {
			best = o
			found = true
		}
	}
	return best, found, nil
}
