//go:build darwin

package applevf

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ImageCached reports whether the pinned VM image for an OCI reference is in
// Lima's download cache with its digest record (what Prepare leaves behind),
// and the image URL. It downloads and verifies nothing; `mycel sandbox
// status` uses it.
func (b *Backend) ImageCached(image string) (bool, string, error) {
	img, err := resolveImage(image, b.goarch)
	if err != nil {
		return false, "", err
	}
	cacheDir, err := b.cacheDir()
	if err != nil {
		return false, img.Location, fmt.Errorf("applevf: lima cache dir: %w", err)
	}
	algo, _, _, err := digestHash(img.Digest)
	if err != nil {
		return false, img.Location, err
	}
	dir := cacheEntryDir(cacheDir, img.Location)
	if _, err := os.Stat(filepath.Join(dir, "data")); err != nil {
		return false, img.Location, nil
	}
	got, err := os.ReadFile(filepath.Join(dir, algo+".digest"))
	if err != nil {
		return false, img.Location, nil
	}
	return strings.TrimSpace(string(got)) == img.Digest, img.Location, nil
}
