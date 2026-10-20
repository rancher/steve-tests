// Package indexedfields checks SQL field indexes against fixtures it owns.
package indexedfields

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/rancher/steve-tests/actions"
)

// Add a row for a new indexed field. Supply two valid resource bodies whose
// field values sort low < high under the index's type, and an update that changes low to high.
// The same contract checks filtering, both sort directions, and index refresh.
func TestSQLIndexedFields(t *testing.T) {
	t.Parallel()
	configMapLow, configMapHigh := 2, 10
	if !actions.SteveSupports(actions.SteveNumericConfigMapCount) {
		configMapLow, configMapHigh = 10, 2
	}
	tests := []struct {
		name, resource, field string
		low, high             map[string]any
		lowValue, highValue   string
		update                func(map[string]any) error
		feature               actions.SteveFeature
	}{
		// HPA replica indexes use TEXT, so "10" sorts before "2".
		{
			name: "hpa min replicas", resource: "autoscaling.horizontalpodautoscaler",
			field: "spec.minReplicas",
			low:   hpa(10, 20), high: hpa(2, 20),
			lowValue: "10", highValue: "2",
			update:  updateSpecField("minReplicas", 2),
			feature: actions.SteveNumericFieldIndexes,
		},
		{
			name: "hpa max replicas", resource: "autoscaling.horizontalpodautoscaler",
			field: "spec.maxReplicas",
			low:   hpa(1, 10), high: hpa(1, 2),
			lowValue: "10", highValue: "2",
			update:  updateSpecField("maxReplicas", 2),
			feature: actions.SteveNumericFieldIndexes,
		},
		// ConfigMap data counts use INT on newer releases and TEXT on older ones.
		{
			name: "configmap data count", resource: "configmap",
			field: "metadata.fields[1]",
			low:   configMap(configMapLow), high: configMap(configMapHigh),
			lowValue: fmt.Sprint(configMapLow), highValue: fmt.Sprint(configMapHigh),
			update: func(current map[string]any) error {
				current["data"] = configMap(configMapHigh)["data"]
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.feature != "" {
				actions.RequireSteveFeature(t, test.feature)
			}
			cl := actions.RequireSQLMode(t).Client
			if _, ok := cl.Schema(test.resource); !ok {
				t.Fatalf("required schema %q is missing", test.resource)
			}
			ns := actions.CreateTestNamespace(t, cl)
			resources := cl.Resource(test.resource).Namespace(ns)
			// Reverse alphabetical names catch a sort that silently uses name instead
			// of the requested field. 2 and 10 also distinguish numeric and text order.
			for _, fixture := range []struct {
				name string
				body map[string]any
			}{
				{"z-low", test.low},
				{"a-high", test.high},
			} {
				metadata, _ := fixture.body["metadata"].(map[string]any)
				if metadata == nil {
					metadata = make(map[string]any)
				}
				metadata["name"], metadata["namespace"] = fixture.name, ns
				labels, _ := metadata["labels"].(map[string]string)
				labels = actions.ManagedLabels(labels)
				labels[actions.CaseLabel] = "indexed-field"
				metadata["labels"] = labels
				fixture.body["metadata"] = metadata
				actions.CreateAndWait(t, resources, fixture.name, fixture.body)
			}
			check := func(t *testing.T, filter, sort string, want []string) {
				t.Helper()
				values := url.Values{
					"filter": {"metadata.labels." + actions.CaseLabel + "=indexed-field"},
					"sort":   {sort},
				}
				if filter != "" {
					values.Add("filter", filter)
				}
				actions.WaitForCollection(t, resources, values.Encode(), actions.CollectionExpectation{Order: want})
			}
			for _, assertion := range []struct {
				name, filter, sort string
				want               []string
			}{
				{"filter low", test.field + "=" + test.lowValue, "metadata.name", []string{"z-low"}},
				{"filter high", test.field + "=" + test.highValue, "metadata.name", []string{"a-high"}},
				{"ascending", "", test.field + ",metadata.name", []string{"z-low", "a-high"}},
				{"descending", "", "-" + test.field + ",metadata.name", []string{"a-high", "z-low"}},
			} {
				t.Run(assertion.name, func(t *testing.T) {
					check(t, assertion.filter, assertion.sort, assertion.want)
				})
			}
			// This subtest performs its own transition and does not rely on siblings.
			t.Run("update refreshes index", func(t *testing.T) {
				check(t, test.field+"="+test.lowValue, "metadata.name", []string{"z-low"})
				if _, err := actions.UpdateWithConflictRetry(t.Context(), resources, "z-low", test.update); err != nil {
					t.Fatalf("update indexed field: %v", err)
				}
				check(t, test.field+"="+test.highValue, "metadata.name", []string{"a-high", "z-low"})
				check(t, test.field+"="+test.lowValue, "metadata.name", []string{})
				check(t, "", test.field+",metadata.name", []string{"a-high", "z-low"})
			})
		})
	}
}

func updateSpecField(field string, value any) func(map[string]any) error {
	return func(current map[string]any) error {
		spec, ok := current["spec"].(map[string]any)
		if !ok {
			return fmt.Errorf("resource has no spec object")
		}
		spec[field] = value
		return nil
	}
}
