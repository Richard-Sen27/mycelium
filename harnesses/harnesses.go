// Package harnesses embeds the harness manifests (*.yaml in this directory)
// into the mycel binary. The manifest format is described in README.md.
package harnesses

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.yaml
var files embed.FS

// Names returns the embedded manifest names, sorted.
func Names() []string {
	entries, _ := fs.ReadDir(files, ".")
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Read returns the raw manifest for name, or an error wrapping fs.ErrNotExist.
func Read(name string) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return nil, fs.ErrNotExist
	}
	return files.ReadFile(name + ".yaml")
}
