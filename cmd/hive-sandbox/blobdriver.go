package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
)

// blobConfig is how the operator picks a backend (D11: the driver is chosen at
// config time, not per call).
//
// Disk is the default because it makes development a `go run` rather than a
// Garage cluster, and because the seam exists precisely so the choice costs
// nothing. Both drivers record their Name() on the blobs row, so objects
// written under one remain findable after a switch -- what a switch does NOT do
// is move bytes, which is why migrating between them is a separate operation
// rather than a config change.
type blobConfig struct {
	driver string // "disk" or "s3"
	root   string // disk only
}

// blobDriver builds the configured driver.
//
// S3 credentials come from the environment rather than flags: a flag is visible
// in `ps` to every user on the box, and a secret that leaks through process
// listing leaks silently.
func blobDriver(cfg blobConfig) (blob.Driver, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.driver)) {
	case "", "disk":
		return blob.NewDiskDriver(cfg.root)

	case "s3":
		s3 := blob.S3Config{
			Endpoint:        os.Getenv("HIVE_SANDBOX_S3_ENDPOINT"),
			Bucket:          os.Getenv("HIVE_SANDBOX_S3_BUCKET"),
			Region:          os.Getenv("HIVE_SANDBOX_S3_REGION"),
			AccessKeyID:     os.Getenv("HIVE_SANDBOX_S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("HIVE_SANDBOX_S3_SECRET_ACCESS_KEY"),
			Prefix:          os.Getenv("HIVE_SANDBOX_S3_PREFIX"),
		}
		// Named individually rather than as one "s3 is misconfigured": an
		// operator with four variables set and one missing should be told
		// which, not made to bisect their own environment.
		var missing []string
		if s3.Endpoint == "" {
			missing = append(missing, "HIVE_SANDBOX_S3_ENDPOINT")
		}
		if s3.Bucket == "" {
			missing = append(missing, "HIVE_SANDBOX_S3_BUCKET")
		}
		if s3.AccessKeyID == "" {
			missing = append(missing, "HIVE_SANDBOX_S3_ACCESS_KEY_ID")
		}
		if s3.SecretAccessKey == "" {
			missing = append(missing, "HIVE_SANDBOX_S3_SECRET_ACCESS_KEY")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("blob driver s3: missing %s", strings.Join(missing, ", "))
		}
		return blob.NewS3Driver(s3)

	default:
		return nil, fmt.Errorf("unknown blob driver %q: want disk or s3", cfg.driver)
	}
}
