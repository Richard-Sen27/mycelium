package bundle

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// FilterStats counts what FilterRootfs dropped or changed.
type FilterStats struct {
	Kept, Dropped, Stripped int
}

// FilterRootfs copies the `docker export` tar stream from r to w, keeping
// only entries that are safe to extract as root on the sandbox host:
//
//   - regular files, directories, symlinks and hard links only (character
//     and block devices, FIFOs and anything else are dropped);
//   - names must be relative and free of ".." (absolute or escaping names
//     are dropped);
//   - no entry below a symlink seen earlier in the stream (tar would
//     otherwise write through the link onto the host);
//   - hard links must point at a relative, ".."-free name already kept;
//   - setuid and setgid bits are stripped.
//
// Symlink targets are not checked: they are resolved by the sandbox, inside
// its own root, never by the host (the rootfs is never written through after
// extraction).
func FilterRootfs(r io.Reader, w io.Writer) (FilterStats, error) {
	var st FilterStats
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	links := map[string]bool{}
	kept := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("bundle: read image export: %w", err)
		}
		name, ok := cleanTarName(h.Name)
		if !ok || underLink(name, links) {
			st.Dropped++
			continue
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink: //nolint:staticcheck // TypeRegA is folded into TypeReg by archive/tar
		case tar.TypeLink:
			target, ok := cleanTarName(h.Linkname)
			if !ok || !kept[target] {
				st.Dropped++
				continue
			}
			h.Linkname = target
		default:
			st.Dropped++
			continue
		}
		if h.Mode&0o6000 != 0 {
			h.Mode &^= 0o6000
			st.Stripped++
		}
		h.Name = name
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		h.PAXRecords = nil
		h.Xattrs = nil //nolint:staticcheck // clear legacy xattrs too
		h.Format = tar.FormatUnknown
		if err := tw.WriteHeader(h); err != nil {
			return st, err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil { // #nosec G110 -- size is bounded by the image
				return st, err
			}
		}
		if h.Typeflag == tar.TypeSymlink {
			links[name] = true
		}
		kept[name] = true
		st.Kept++
	}
	return st, tw.Close()
}

// cleanTarName normalises an entry name and refuses absolute or ".." names.
func cleanTarName(n string) (string, bool) {
	n = strings.TrimPrefix(n, "./")
	if n == "" || strings.HasPrefix(n, "/") || strings.ContainsRune(n, 0) {
		return "", false
	}
	for _, el := range strings.Split(strings.TrimSuffix(n, "/"), "/") {
		if el == ".." {
			return "", false
		}
	}
	c := path.Clean(n)
	if c == "." {
		return "", false
	}
	return c, true
}

func underLink(name string, links map[string]bool) bool {
	for p := path.Dir(name); p != "." && p != "/"; p = path.Dir(p) {
		if links[p] {
			return true
		}
	}
	return false
}
