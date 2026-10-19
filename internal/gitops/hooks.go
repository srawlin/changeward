package gitops

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// hookAnnotations mark resources a GitOps controller runs around a sync
// (Argo CD hooks, Helm hooks) rather than reconciling as desired state.
var hookAnnotations = []string{"argocd.argoproj.io/hook", "helm.sh/hook"}

// StripHooks removes hook resources (such as the gate's own PreSync Job, which
// uses generateName and would make every plan different) from a multi-document
// manifest stream. It returns the remaining documents and how many were removed.
func StripHooks(manifests string) (string, int, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifests))
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	removed := 0
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, err
		}
		if len(doc.Content) == 0 {
			continue // empty document
		}
		var meta struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		_ = doc.Decode(&meta)
		hook := false
		for _, a := range hookAnnotations {
			if _, ok := meta.Metadata.Annotations[a]; ok {
				hook = true
			}
		}
		if hook {
			removed++
			continue
		}
		if err := enc.Encode(&doc); err != nil {
			return "", 0, err
		}
	}
	if err := enc.Close(); err != nil {
		return "", 0, err
	}
	return out.String(), removed, nil
}
