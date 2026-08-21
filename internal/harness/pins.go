package harness

import (
	"encoding/json"
	"fmt"
	"os"
)

// DefaultPinsPath is where scripts/harness-build.sh writes, relative to the
// repo root.
const DefaultPinsPath = "docker/harness/digests.json"

// ImagePin is one runtime's pinned image.
type ImagePin struct {
	Digest     string `json:"digest"`
	CLIVersion string `json:"cli_version"`
}

// ImagePins is the lockfile: which digest each runtime runs at.
//
// Runs pin a digest, never a tag (D12.5). Upgrading a harness is rebuilding and
// committing a changed digest here, which makes "which CLI built this app"
// answerable from git history rather than from memory.
type ImagePins struct {
	Repository string               `json:"repository"`
	Runtimes   map[Runtime]ImagePin `json:"runtimes"`
}

// LoadPins reads a lockfile.
func LoadPins(path string) (ImagePins, error) {
	// The path is operator configuration, not run input: nothing a spec or a
	// guest controls reaches it.
	raw, err := os.ReadFile(path) //nolint:gosec // G304: caller-supplied config path by design
	if err != nil {
		return ImagePins{}, fmt.Errorf("read harness pins: %w", err)
	}

	var pins ImagePins
	if err := json.Unmarshal(raw, &pins); err != nil {
		return ImagePins{}, fmt.Errorf("parse harness pins %s: %w", path, err)
	}
	if pins.Repository == "" {
		pins.Repository = DefaultImageRepository
	}
	return pins, nil
}

// Apply fills a spec's image fields from the pins for its runtime. It is the
// only intended way to populate ImageDigest, so a caller cannot casually run
// something unpinned.
func (p ImagePins) Apply(spec *RunSpec) error {
	pin, ok := p.Runtimes[spec.Runtime]
	if !ok {
		return fmt.Errorf("harness: no pinned image for runtime %q; run scripts/harness-build.sh", spec.Runtime)
	}
	if pin.Digest == "" {
		return fmt.Errorf("harness: pin for runtime %q has no digest", spec.Runtime)
	}

	spec.ImageRepository = p.Repository
	spec.ImageDigest = pin.Digest
	spec.CLIVersion = pin.CLIVersion
	return nil
}
