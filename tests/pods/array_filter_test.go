package pods

import (
	"fmt"
	"testing"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/query"
)

// CONTAINS and NOTCONTAINS compare complete array members and inspect every
// container, including those after the first.
func TestPodsArrayFilterByExactContainerImage(t *testing.T) {
	actions.RequireSteveFeature(t, actions.SteveArrayMembershipFilters)
	setup := actions.RequireSQLMode(t)
	actions.RequireWorkloads(t)
	cl := setup.Client
	ns := actions.CreateTestNamespace(t, cl)
	resources := cl.Resource("pod").Namespace(ns)
	caseID := actions.UniqueName(t, "pod-exact-image")
	labels := map[string]string{actions.CaseLabel: caseID}

	// These values are deliberately fixed synthetic index data. This test only
	// waits for Steve collection visibility, not for the containers to start, so
	// the workload image override is reserved for fixtures that must actually run.
	const (
		targetImage    = "busybox:1.36"
		substringImage = "busybox:1.36-extra"
		otherImage     = "busybox:1.35"
	)
	seeds := []struct {
		name   string
		images []string
	}{
		// The matching value is intentionally in the second container.
		{name: "exact-second", images: []string{otherImage, targetImage}},
		{name: "substring-only", images: []string{substringImage}},
		{name: "other", images: []string{otherImage}},
	}
	for _, seed := range seeds {
		actions.CreateAndWait(t, resources, seed.name, newArrayFilterPodBody(ns, seed.name, labels, seed.images))
	}

	tests := []struct {
		name       string
		expression string
		want       []string
	}{
		{
			name:       "CONTAINS exact member in non-first container",
			expression: query.Contains("spec.containers.image", targetImage),
			want:       []string{"exact-second"},
		},
		{
			name:       "NOTCONTAINS excludes exact member only",
			expression: query.NotContains("spec.containers.image", targetImage),
			want:       []string{"other", "substring-only"},
		},
		{
			name:       "CONTAINS does not treat a prefix as a member",
			expression: query.Contains("spec.containers.image", substringImage),
			want:       []string{"substring-only"},
		},
	}

	encode := func(tb *testing.T, expression string) string {
		tb.Helper()
		encoded, err := (query.ListOptions{
			Filters: []string{
				query.Equal("metadata.labels."+actions.CaseLabel, caseID),
				expression,
			},
			Sort: []string{"metadata.name"},
		}).Encode()
		if err != nil {
			tb.Fatalf("encode Pod list query: %v", err)
		}
		return encoded
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			coll, err := resources.List(encode(t, tc.expression))
			if err != nil {
				t.Fatalf("list Pods with %q: %v", tc.expression, err)
			}
			actions.AssertCollectionLength(t, coll, len(tc.want))
			actions.AssertCollectionOrder(t, coll, tc.want)
		})
	}
}

func newArrayFilterPodBody(ns string, name string, labels map[string]string, images []string) map[string]any {
	containers := make([]map[string]any, 0, len(images))
	for index, image := range images {
		containers = append(containers, map[string]any{
			"name":            fmt.Sprintf("image-%d", index),
			"image":           image,
			"imagePullPolicy": "IfNotPresent",
			"command":         []string{"sh", "-c", "sleep 3600"},
		})
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    actions.ManagedLabels(labels),
		},
		"spec": map[string]any{
			"restartPolicy":                 "Never",
			"terminationGracePeriodSeconds": int64(0),
			"containers":                    containers,
		},
	}
}
