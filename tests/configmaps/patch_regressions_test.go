package configmaps

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/rancher/steve-tests/actions"
)

// A namespaced PATCH may omit metadata.namespace when it is in the URL, and
// Steve must preserve the requested Kubernetes patch media type.
func TestNamespacedPatchPreservesPatchType(t *testing.T) {
	t.Parallel()
	cl := getTestClient(t)
	actions.RequireSteveFeature(t, actions.SteveNamespacedPatch)
	ns := actions.CreateTestNamespace(t, cl)
	resources := cl.Resource("configmap").Namespace(ns)

	tests := []struct {
		name        string
		contentType string
		patch       any
		wantValue   string
	}{
		{
			name:        "strategic merge patch",
			contentType: "application/strategic-merge-patch+json",
			patch: map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"patch-probe": "strategic"},
				},
			},
			wantValue: "strategic",
		},
		{
			name:        "merge patch",
			contentType: "application/merge-patch+json",
			patch: map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"patch-probe": "merge"},
				},
			},
			wantValue: "merge",
		},
		{
			name:        "JSON patch",
			contentType: "application/json-patch+json",
			patch: []map[string]any{
				{
					"op":    "replace",
					"path":  "/metadata/labels/patch-probe",
					"value": "json",
				},
			},
			wantValue: "json",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("patch-%d", i)
			initial := actions.ManagedLabels(map[string]string{
				actions.CaseLabel: tc.wantValue,
				"patch-probe":     "before",
			})
			actions.CreateAndWait(t, resources, name, newConfigMapBody(ns, name, initial))

			// Deliberately do not add metadata.namespace to any patch body. The
			// URL namespace is authoritative for this request.
			if _, err := resources.Patch(name, tc.patch, tc.contentType); err != nil {
				t.Fatalf("%s failed: %v", tc.contentType, err)
			}

			got, err := resources.Get(name)
			if err != nil {
				t.Fatalf("get patched ConfigMap: %v", err)
			}
			wantLabels := cloneLabels(initial)
			wantLabels["patch-probe"] = tc.wantValue
			if gotLabels := labelsFromResource(t, got); !reflect.DeepEqual(gotLabels, wantLabels) {
				t.Fatalf("labels after %s = %#v, want exactly %#v", tc.contentType, gotLabels, wantLabels)
			}
		})
	}
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func labelsFromResource(t *testing.T, resource map[string]any) map[string]string {
	t.Helper()
	metadata, ok := resource["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("resource metadata has type %T, want object", resource["metadata"])
	}
	rawLabels, ok := metadata["labels"].(map[string]any)
	if !ok {
		t.Fatalf("resource metadata.labels has type %T, want object", metadata["labels"])
	}
	labels := make(map[string]string, len(rawLabels))
	for key, raw := range rawLabels {
		value, ok := raw.(string)
		if !ok {
			t.Fatalf("resource label %q has type %T, want string", key, raw)
		}
		labels[key] = value
	}
	return labels
}
