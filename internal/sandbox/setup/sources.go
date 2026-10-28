package setup

// Source is one pinned download: the file is used only when its digest
// matches. Refresh URL, version and digest together (docs/decisions.md D026).
type Source struct {
	Name    string // human name for the log
	Version string
	URL     string
	File    string // file name in <Dir>/downloads
	Digest  string // "sha256:<hex>" or "sha512:<hex>"
	SizeMB  int64  // approximate, for the confirmation question
}

// AlpineRootfs is the root filesystem of the mycel-sbx WSL distro: Alpine's
// official "mini root filesystem" (3.7 MB, apk and busybox only), from the
// release index dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/
// latest-releases.yaml (sha256 as published there and in the .sha256 file).
// The packages the backend needs come from Alpine's signed repositories
// listed in its /etc/apk/repositories (v3.24 main and community).
var AlpineRootfs = Source{
	Name:    "Alpine Linux minimal root filesystem",
	Version: "3.24.3",
	URL:     "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/alpine-minirootfs-3.24.3-x86_64.tar.gz",
	File:    "alpine-minirootfs-3.24.3-x86_64.tar.gz",
	Digest:  "sha256:f8e1cafba9b1b283b5d30d06f9885c8d2bc161ffe9083ade8e2e157813a53407",
	SizeMB:  4,
}

// AlpineBranch is the Alpine release branch the distro's packages come from.
const AlpineBranch = "v3.24"

// AlpineRepositories is the distro's /etc/apk/repositories, pinned to the
// branch of AlpineRootfs (apk verifies the indexes with the signing keys in
// the root filesystem).
const AlpineRepositories = "https://dl-cdn.alpinelinux.org/alpine/" + AlpineBranch + "/main\n" +
	"https://dl-cdn.alpinelinux.org/alpine/" + AlpineBranch + "/community\n"

// AlpinePackages are the Alpine packages that provide bundle.DistroRequirements
// besides runsc: ip (iproute2, with netns), nft, setsid --wait
// (util-linux-misc), du -B1 (coreutils), GNU tar and find.
var AlpinePackages = []string{"iproute2", "nftables", "util-linux-misc", "coreutils", "tar", "findutils"}

// GVisorVersion is the pinned gVisor release, as `runsc --version` prints it.
const GVisorVersion = "release-20261012.0"

// GVisor is the pinned gVisor release per Go architecture. Since mid-2026
// gVisor publishes each release as gvisor.tar.bz2 (runsc plus the sidecar
// binaries in gvisor-bin/, which runsc requires next to itself) with a
// .sha512 file; the digests below are those published files.
var GVisor = map[string]Source{
	"amd64": {
		Name:    "gVisor (runsc)",
		Version: GVisorVersion,
		URL:     "https://storage.googleapis.com/gvisor/releases/release/20261012.0/x86_64/gvisor.tar.bz2",
		File:    "gvisor-20261012.0-x86_64.tar.bz2",
		Digest:  "sha512:9606445eed5aedfcf05310e7ccef61d994089914c6622fc82624c24d2b18db1b2210f40e40a7be27b8b7772859de7d95c501b4eba66423053c960bf671377c5e",
		SizeMB:  101,
	},
	"arm64": {
		Name:    "gVisor (runsc)",
		Version: GVisorVersion,
		URL:     "https://storage.googleapis.com/gvisor/releases/release/20261012.0/aarch64/gvisor.tar.bz2",
		File:    "gvisor-20261012.0-aarch64.tar.bz2",
		Digest:  "sha512:2fe7425abd16ad6ca649ab9bc3d7e8e76756834d86ffcc08dbe97c82c3b7262a7d6401f23632f2b296649cc1408b560416b2ab65a4043aa123fc7b90f3a1b2e2",
		SizeMB:  93,
	},
}
