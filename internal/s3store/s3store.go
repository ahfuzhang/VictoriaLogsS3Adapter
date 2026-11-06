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
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
)

// Environment variables holding S3 credentials.
const (
	EnvAccessKeyID     = "auth.access_key_id"
	EnvSecretAccessKey = "auth.secret_access_key"
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
		"${FORMAT} is replaced with the backup start time (UTC), where FORMAT combines YYYY (year), MM (month), DD (day), "+
		"HH (hour), MI (minute), SS (second), each at most once, optionally separated by '-', '_' or '.', "+
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
	mc     *minio.Client
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
	mc, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: u.Scheme == "https",
		Region: cfg.Region,
		// BucketLookupAuto uses virtual-hosted style for AWS, GCS and Aliyun OSS
		// endpoints and path style for everything else (e.g. MinIO).
		BucketLookup: minio.BucketLookupAuto,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create S3 client for %q: %w", cfg.Endpoint, err)
	}
	c := &Client{mc: mc, bucket: cfg.Bucket}
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Bucket returns the bucket name.
func (c *Client) Bucket() string { return c.bucket }

// Check verifies that the bucket exists and is accessible with the configured credentials.
func (c *Client) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("cannot access S3 bucket %q: %w", c.bucket, err)
	}
	if !ok {
		return fmt.Errorf("S3 bucket %q does not exist", c.bucket)
	}
	return nil
}

// Upload streams r into the object key using multipart upload with an unknown size.
// The object becomes visible only after the whole stream is uploaded successfully.
// It returns the number of uploaded bytes.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, contentType string) (int64, error) {
	info, err := c.mc.PutObject(ctx, c.bucket, key, r, -1, minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    DefaultPartSize,
		// Use UNSIGNED-PAYLOAD instead of aws-chunked streaming signatures for plain http
		// endpoints. Some S3-compatible stores do not decode aws-chunked bodies and would
		// store chunk signatures as part of the object.
		DisableContentSha256: true,
	})
	if err != nil {
		return 0, fmt.Errorf("cannot upload s3://%s/%s: %w", c.bucket, key, err)
	}
	return info.Size, nil
}

// Open opens the object key for reading. The caller must close the returned reader.
func (c *Client) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("cannot open s3://%s/%s: %w", c.bucket, key, err)
	}
	st, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, 0, fmt.Errorf("cannot stat s3://%s/%s: %w", c.bucket, key, err)
	}
	return obj, st.Size, nil
}

// Stat returns object info. found is false when the object does not exist.
func (c *Client) Stat(ctx context.Context, key string) (info ObjectInfo, found bool, err error) {
	st, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, false, nil
		}
		return ObjectInfo{}, false, fmt.Errorf("cannot stat s3://%s/%s: %w", c.bucket, key, err)
	}
	return ObjectInfo{Key: st.Key, Size: st.Size, LastModified: st.LastModified}, true, nil
}

// List returns all objects whose keys start with prefix (recursively).
func (c *Client) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for obj := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("cannot list s3://%s/%s: %w", c.bucket, prefix, obj.Err)
		}
		out = append(out, ObjectInfo{Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified})
	}
	return out, nil
}

// Delete removes the object key.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("cannot delete s3://%s/%s: %w", c.bucket, key, err)
	}
	return nil
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == 404 || resp.Code == "NoSuchKey" || resp.Code == "NotFound"
}

// BackupFileName returns the backup object base name for the partition date (YYYYMMDD).
func BackupFileName(date string) string {
	return date + ".tar"
}

// BackupKey returns the object key for the given partition date using the
// prefix template expanded at time now.
func BackupKey(tmpl *pathtmpl.Template, now time.Time, date string) string {
	return pathtmpl.Join(tmpl.Expand(now), BackupFileName(date))
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
		return c.Stat(ctx, pathtmpl.Join(tmpl.Expand(time.Time{}), name))
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
