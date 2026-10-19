package kubernetes

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestFile is a manifest to write (or remove) when promoting a change to Git.
type ManifestFile struct {
	Path   string // relative to the manifests directory: <namespace>/<kind>-<name>.yaml
	YAML   string
	Delete bool
}

func fileFor(r Ref) string {
	ns := r.Namespace
	if ns == "" {
		ns = "_cluster"
	}
	return filepath.Join(ns, strings.ToLower(r.Kind)+"-"+r.Name+".yaml")
}

// ManifestsForPromotion turns an applied plan into the desired-state
// manifests to commit: created and updated objects are written as applied;
// deleted objects' files are removed.
func ManifestsForPromotion(adapterData []byte) ([]ManifestFile, error) {
	var d planData
	if err := json.Unmarshal(adapterData, &d); err != nil {
		return nil, err
	}
	var out []ManifestFile
	for _, s := range d.Steps {
		switch s.Action {
		case "unchanged":
			continue
		case "delete":
			out = append(out, ManifestFile{Path: fileFor(s.Ref), Delete: true})
		default:
			o := strip(s.Desire)
			if md, ok := o["metadata"].(map[string]any); ok {
				delete(md, "annotations") // drop server/tool annotations; the desired manifest is what matters
				if s.Desire != nil {
					if dm, ok := s.Desire["metadata"].(map[string]any); ok && dm["annotations"] != nil {
						md["annotations"] = dm["annotations"]
					}
				}
			}
			b, err := yaml.Marshal(o)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Ref, err)
			}
			out = append(out, ManifestFile{Path: fileFor(s.Ref), YAML: string(b)})
		}
	}
	return out, nil
}
