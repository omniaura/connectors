// Package manifests embeds the connector manifests maintained in this
// repository.
package manifests

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/omniaura/connectors"
)

//go:embed *.yaml
var files embed.FS

// Get returns the manifest for slug.
func Get(slug string) (connectors.Manifest, error) {
	data, err := files.ReadFile(slug + ".yaml")
	if err != nil {
		return connectors.Manifest{}, fmt.Errorf("no manifest for %q", slug)
	}
	return connectors.ParseManifest(data)
}

// Slugs lists the embedded manifests.
func Slugs() []string {
	entries, _ := fs.ReadDir(files, ".")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
