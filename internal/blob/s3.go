package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config points a driver at an S3-compatible store.
//
// Garage is the intended backend (D6), and everything here is plain S3, so a
// different implementation is a config change rather than a code change.
type S3Config struct {
	// Endpoint is the base URL, e.g. http://127.0.0.1:3900.
	Endpoint string

	Bucket string
	Region string

	AccessKeyID     string
	SecretAccessKey string

	// Prefix is joined ahead of every content address, so one bucket can hold
	// more than this platform. Optional.
	Prefix string

	// MaxPresignTTL bounds a signed URL. Defaults to DefaultMaxPresignTTL.
	MaxPresignTTL time.Duration

	// HTTPClient is used for both the API and presign paths. Optional.
	HTTPClient *http.Client
}

// DefaultMaxPresignTTL is deliberately short. A signed URL is a bearer token
// for bytes, and the client cache means a fresh one is cheap.
const DefaultMaxPresignTTL = 5 * time.Minute

func (c S3Config) validate() error {
	if strings.TrimSpace(c.Endpoint) == "" {
		return errors.New("blob: s3 driver needs an endpoint")
	}
	if strings.TrimSpace(c.Bucket) == "" {
		return errors.New("blob: s3 driver needs a bucket")
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return errors.New("blob: s3 driver needs credentials")
	}
	return nil
}

// S3Driver stores objects in an S3-compatible bucket.
//
// It can presign, which makes it the first driver where [PlanDelivery]'s
// scriptable-MIME rule does any work. The disk driver could not redirect at
// all, so the rule has never been exercised against a backend that can ... and
// a rule nobody has run is a hypothesis.
type S3Driver struct {
	cfg    S3Config
	client *s3.Client
	// presigner is separate because a presigned request is built, not sent.
	presigner *s3.PresignClient
}

// NewS3Driver builds a driver from config.
func NewS3Driver(cfg S3Config) (*S3Driver, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		// Garage ignores the region but the signer does not: it goes into the
		// credential scope, so it has to match what the server expects.
		cfg.Region = "garage"
	}
	if cfg.MaxPresignTTL <= 0 {
		cfg.MaxPresignTTL = DefaultMaxPresignTTL
	}

	options := s3.Options{
		Region: cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		BaseEndpoint: aws.String(strings.TrimRight(cfg.Endpoint, "/")),
		// Garage serves one endpoint for every bucket; virtual-host addressing
		// would turn the bucket into a DNS label nobody has published.
		UsePathStyle: true,
	}
	if cfg.HTTPClient != nil {
		options.HTTPClient = cfg.HTTPClient
	}

	client := s3.New(options)
	return &S3Driver{
		cfg:       cfg,
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func (d *S3Driver) Name() string { return "s3" }

// Caps reports presigning, which is the switch the whole delivery decision is
// shaped around.
func (d *S3Driver) Caps() Caps {
	return Caps{Presign: true, MaxPresignTTL: d.cfg.MaxPresignTTL}
}

// key is the object's full key: the optional prefix plus the content address.
func (d *S3Driver) key(h Hash) string {
	if d.cfg.Prefix == "" {
		return h.Key()
	}
	return strings.TrimRight(d.cfg.Prefix, "/") + "/" + h.Key()
}

func (d *S3Driver) Stat(ctx context.Context, h Hash) (ObjectInfo, error) {
	if h.IsZero() {
		return ObjectInfo{}, fmt.Errorf("%w: zero hash", ErrMalformedHash)
	}

	out, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.cfg.Bucket),
		Key:    aws.String(d.key(h)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return ObjectInfo{}, fmt.Errorf("blob: head %s: %w", h, err)
	}

	info := ObjectInfo{Hash: h}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ETag != nil {
		info.ETag = *out.ETag
	}
	return info, nil
}

func (d *S3Driver) Open(ctx context.Context, h Hash, r Range) (io.ReadCloser, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(d.cfg.Bucket),
		Key:    aws.String(d.key(h)),
	}
	if header := RangeHeader(r); header != "" {
		input.Range = aws.String(header)
	}

	out, err := d.client.GetObject(ctx, input)
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		if isS3RangeNotSatisfiable(err) {
			return nil, ErrRangeNotSatisfiable
		}
		return nil, fmt.Errorf("blob: get %s: %w", h, err)
	}
	return out.Body, nil
}

func (d *S3Driver) CreateUpload(ctx context.Context, spec CreateUpload) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Limit < 0 {
		return nil, errors.New("blob: negative upload limit")
	}

	// Buffered to a temp file rather than streamed straight through.
	//
	// The address is the digest, and the digest is not known until the last
	// byte. Streaming to a temp key and copying on seal is the alternative, and
	// it costs an O(size) server-side copy on every upload; buffering costs
	// local disk, once. The declared-hash fast path skips neither, because the
	// hint is not trusted.
	temp, err := newSpoolFile()
	if err != nil {
		return nil, err
	}

	return &s3Upload{
		driver:   d,
		spool:    temp,
		hasher:   NewHasher(),
		declared: spec.DeclaredHash,
		limit:    spec.Limit,
	}, nil
}

func (d *S3Driver) Delete(ctx context.Context, h Hash) error {
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.cfg.Bucket),
		Key:    aws.String(d.key(h)),
	})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("blob: delete %s: %w", h, err)
	}
	// Idempotent, like the disk driver: a sweeper that stops on its own retry
	// is worse than one that deletes nothing twice.
	return nil
}

// Deliver returns a signed URL for inert content and a proxied body for
// anything a browser can execute.
//
// The rule itself lives in [PlanDelivery], which this consults rather than
// reimplements. What makes that safe is that [PresignGet] refuses scriptable
// types independently: the decision is made once here and enforced again one
// layer down, so a future caller that skips PlanDelivery, or a PlanDelivery that
// grows a bug, still cannot get scriptable bytes signed.
//
// The reason it is worth two layers: a signed URL structurally cannot carry
// `X-Content-Type-Options: nosniff`, because S3 has no response override for
// that header. Scriptable bytes handed out as a URL are a stored-XSS primitive
// with no header available to stop it.
func (d *S3Driver) Deliver(ctx context.Context, req DeliveryRequest) (Delivery, error) {
	if err := ctx.Err(); err != nil {
		return Delivery{}, err
	}

	if PlanDelivery(d.Caps(), req) == DeliverProxy {
		info, err := d.Stat(ctx, req.Hash)
		if err != nil {
			return Delivery{}, err
		}
		clamped, err := req.Range.Clamp(info.Size)
		if err != nil {
			return Delivery{}, err
		}
		body, err := d.Open(ctx, req.Hash, clamped)
		if err != nil {
			return Delivery{}, err
		}

		size := clamped.Length
		if clamped.IsFull() {
			size = info.Size
		}
		return Delivery{Kind: DeliverProxy, Body: body, Size: size}, nil
	}

	url, err := d.PresignGet(ctx, req.Hash, req.MIME, req.TTL)
	if err != nil {
		return Delivery{}, err
	}
	return Delivery{Kind: DeliverRedirect, URL: url}, nil
}

// PresignGet returns a signed URL for an object.
//
// **No Range is signed.** The signature covers `host` and the query, so a
// client may range the same URL as many times as it likes: `Range` is an
// unsigned request header. That is what lets the host hand out one URL and let
// the client decide how to fetch, and it is measured rather than assumed ...
// Garage returns 206 byte-exact for a ranged GET against a presigned URL.
//
// It refuses scriptable content, for the reason on [Deliver].
func (d *S3Driver) PresignGet(ctx context.Context, h Hash, mimeType string, ttl time.Duration) (string, error) {
	if h.IsZero() {
		return "", fmt.Errorf("%w: zero hash", ErrMalformedHash)
	}
	if ScriptableMIME(mimeType) {
		return "", fmt.Errorf(
			"blob: refusing to presign %q: a signed URL cannot carry nosniff, so scriptable content is proxied",
			mimeType)
	}

	signed, err := d.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.cfg.Bucket),
		Key:    aws.String(d.key(h)),
		// Pinning the type the host decided on, rather than letting the stored
		// metadata decide what a browser sees.
		ResponseContentType: aws.String(mimeType),
	}, s3.WithPresignExpires(ClampTTL(d.Caps(), ttl)))
	if err != nil {
		return "", fmt.Errorf("blob: presign %s: %w", h, err)
	}
	return signed.URL, nil
}

// EnsureBucket creates the bucket when it is missing. For development and
// tests; a deployment provisions its bucket out of band.
func (d *S3Driver) EnsureBucket(ctx context.Context) error {
	_, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(d.cfg.Bucket)})
	if err == nil {
		return nil
	}
	if _, err := d.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(d.cfg.Bucket),
	}); err != nil {
		var exists *types.BucketAlreadyOwnedByYou
		if errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("blob: create bucket %s: %w", d.cfg.Bucket, err)
	}
	return nil
}

// isS3NotFound covers the several shapes an S3-compatible store uses for
// absence: a typed NoSuchKey, a typed NotFound from HEAD, and a bare 404 with
// no body, which is what a HEAD against Garage produces.
func isS3NotFound(err error) bool {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func isS3RangeNotSatisfiable(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidRange" ||
			strings.Contains(apiErr.ErrorCode(), "RangeNotSatisfiable")
	}
	return false
}

var _ Driver = (*S3Driver)(nil)
