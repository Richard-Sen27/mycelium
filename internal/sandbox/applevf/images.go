//go:build darwin

package applevf

import (
	"fmt"
	"strings"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// limaImage is one cloud image Lima boots, pinned by digest. Lima verifies
// the digest itself when it downloads the file, and Prepare verifies it when
// it warms the cache.
type limaImage struct {
	Location string // https URL of the qcow2/img file
	Arch     string // Lima arch name: "aarch64" or "x86_64"
	Digest   string // "sha256:<hex>" or "sha512:<hex>"
}

// imageFamily is a distribution release with one image per architecture.
type imageFamily struct {
	Name   string // human name for logs and errors
	Images []limaImage
}

// Pinned images, taken from Lima 2.1.1's templates (_images/debian-12.yaml,
// _images/ubuntu-24.04.yaml). Dated release images disappear from the
// mirrors after some months; refresh this table (URL and digest together)
// when Lima updates its templates.
var (
	debian12 = imageFamily{
		Name: "Debian 12 (bookworm) genericcloud",
		Images: []limaImage{
			{
				Location: "https://cloud.debian.org/images/cloud/bookworm/20260316-2418/debian-12-genericcloud-arm64-20260316-2418.qcow2",
				Arch:     "aarch64",
				Digest:   "sha512:26b8edf24cc8464f334b1dbce6fcc8c789d14258d07ad3c226fdaa7068b665d78fa9888a12557f4e21e3b76ae3eaec069d8183cbec6b5bc145c02513025cd706",
			},
			{
				Location: "https://cloud.debian.org/images/cloud/bookworm/20260316-2418/debian-12-genericcloud-amd64-20260316-2418.qcow2",
				Arch:     "x86_64",
				Digest:   "sha512:9c9363e1b0fa5ba57974b98b108f5170a56e021e91e7ddc4d8f094909edecf8374f77b1eb1ce277e57a63128fe13071f6b5d2b16aa6aab68a0760bcbf627a52d",
			},
		},
	}
	ubuntu2404 = imageFamily{
		Name: "Ubuntu 24.04 (noble) server cloud image",
		Images: []limaImage{
			{
				Location: "https://cloud-images.ubuntu.com/releases/noble/release-20260321/ubuntu-24.04-server-cloudimg-arm64.img",
				Arch:     "aarch64",
				Digest:   "sha256:1ea801e659d2f5035ac294e0faab0aac9b6ba66753df933ba5c7beab0c689bd0",
			},
			{
				Location: "https://cloud-images.ubuntu.com/releases/noble/release-20260321/ubuntu-24.04-server-cloudimg-amd64.img",
				Arch:     "x86_64",
				Digest:   "sha256:5c3ddb00f60bc455dac0862fabe9d8bacec46c33ac1751143c5c3683404b110d",
			},
		},
	}
)

// lookupFamily maps a profile's OCI reference to an image family. The guest
// is a full cloud image of the same distribution release, not the OCI image
// itself: a VM needs a kernel and an init system.
func lookupFamily(ref string) (imageFamily, error) {
	r := strings.TrimSpace(ref)
	if strings.Contains(r, "@") {
		return imageFamily{}, fmt.Errorf("%w: applevf: image %q: digest-pinned references cannot be mapped to a VM image", sandbox.ErrUnavailable, ref)
	}
	r = strings.TrimPrefix(r, "docker.io/")
	r = strings.TrimPrefix(r, "index.docker.io/")
	r = strings.TrimPrefix(r, "library/")
	repo, tag, ok := strings.Cut(r, ":")
	if !ok {
		tag = "latest"
	}
	switch repo {
	case "debian":
		if tag == "bookworm" || strings.HasPrefix(tag, "bookworm-") || tag == "12" || strings.HasPrefix(tag, "12-") || strings.HasPrefix(tag, "12.") {
			return debian12, nil
		}
	case "ubuntu":
		if tag == "24.04" || tag == "latest" || strings.HasPrefix(tag, "noble") {
			return ubuntu2404, nil
		}
	}
	return imageFamily{}, fmt.Errorf("%w: applevf: no VM image is known for %q (supported: docker.io/library/debian:bookworm*, ubuntu:24.04, ubuntu:noble*, ubuntu:latest)", sandbox.ErrUnavailable, ref)
}

// limaArch is Lima's name for the host architecture.
func limaArch(goarch string) (string, error) {
	switch goarch {
	case "arm64":
		return "aarch64", nil
	case "amd64":
		return "x86_64", nil
	}
	return "", fmt.Errorf("%w: applevf: unsupported architecture %s", sandbox.ErrUnavailable, goarch)
}

// resolveImage returns the pinned image for ref on goarch (Go's name).
func resolveImage(ref, goarch string) (limaImage, error) {
	fam, err := lookupFamily(ref)
	if err != nil {
		return limaImage{}, err
	}
	arch, err := limaArch(goarch)
	if err != nil {
		return limaImage{}, err
	}
	for _, img := range fam.Images {
		if img.Arch == arch {
			return img, nil
		}
	}
	return limaImage{}, fmt.Errorf("%w: applevf: %s has no %s image", sandbox.ErrUnavailable, fam.Name, arch)
}
