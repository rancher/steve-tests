// Package indexedfields checks SQL field indexes against fixtures it owns.
package indexedfields

import (
	"fmt"
	"net/url"
	"strconv"
	"testing"

	"github.com/rancher/steve-tests/actions"
)

// Add a row for a new indexed field. Supply two valid resource bodies whose
// field values sort low < high under the index's type, and an update that changes low to high.
// The same contract checks filtering, both sort directions, and index refresh.
// Immutable fields omit update and check index refresh after deletion instead.
func TestSQLIndexedFields(t *testing.T) {
	t.Parallel()
	configMapLow, configMapHigh := 2, 10
	group := actions.UniqueName(t, "indexed") + ".steve-tests.invalid"
	lowGroup, highGroup := "alpha."+group, "bravo."+group
	if !actions.SteveSupports(actions.SteveNumericConfigMapCount) {
		configMapLow, configMapHigh = 10, 2
	}
	tests := []struct {
		name, resource, field string
		low, high             map[string]any
		lowValue, highValue   string
		update, remove        func(map[string]any) error
		feature               actions.SteveFeature
	}{
		// HPA replica indexes use TEXT, so "10" sorts before "2".
		{
			name: "hpa min replicas", resource: "autoscaling.horizontalpodautoscaler",
			field: "spec.minReplicas",
			low:   hpa(10, 20), high: hpa(2, 20),
			lowValue: "10", highValue: "2",
			update:  updateField(2, "spec", "minReplicas"),
			feature: actions.SteveNumericFieldIndexes,
		},
		{
			name: "hpa max replicas", resource: "autoscaling.horizontalpodautoscaler",
			field: "spec.maxReplicas",
			low:   hpa(1, 10), high: hpa(1, 2),
			lowValue: "10", highValue: "2",
			update:  updateField(2, "spec", "maxReplicas"),
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
		// rancher/rancher#55811. Different groups allow singular names to converge
		// without colliding with another CRD in the same API group.
		{
			name: "crd group", resource: crdResource, field: "spec.group",
			low:      customResourceDefinition("zlows", lowGroup, "low"),
			high:     customResourceDefinition("ahighs", highGroup, "high"),
			lowValue: lowGroup, highValue: highGroup,
			feature: actions.SteveAdditionalFieldIndexes,
			// spec.group is immutable; the contract checks deletion instead.
		},
		{
			name: "crd singular", resource: crdResource, field: "spec.names.singular",
			low:      customResourceDefinition("zsingulars", lowGroup, "alpha"),
			high:     customResourceDefinition("asingulars", highGroup, "bravo"),
			lowValue: "alpha", highValue: "bravo",
			update:  updateField("bravo", "spec", "names", "singular"),
			feature: actions.SteveAdditionalFieldIndexes,
		},
		{
			name: "configmap description", resource: "configmap", field: descriptionField,
			low:      withDescription(configMap(1), "alpha description"),
			high:     withDescription(configMap(1), "bravo description"),
			lowValue: "alpha description", highValue: "bravo description",
			update:  updateField("bravo description", "metadata", "annotations", descriptionAnnotation),
			remove:  updateField(nil, "metadata", "annotations", descriptionAnnotation),
			feature: actions.SteveAdditionalFieldIndexes,
		},
		{
			name: "crd description", resource: crdResource, field: descriptionField,
			low:      withDescription(customResourceDefinition("zdescriptions", lowGroup, "lowdescription"), "alpha description"),
			high:     withDescription(customResourceDefinition("adescriptions", highGroup, "highdescription"), "bravo description"),
			lowValue: "alpha description", highValue: "bravo description",
			update:  updateField("bravo description", "metadata", "annotations", descriptionAnnotation),
			remove:  updateField(nil, "metadata", "annotations", descriptionAnnotation),
			feature: actions.SteveAdditionalFieldIndexes,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.feature != "" {
				actions.RequireSteveFeature(t, test.feature)
			}
			cl := actions.RequireSQLMode(t).Client
			schema, ok := cl.Schema(test.resource)
			if !ok {
				t.Fatalf("required schema %q is missing", test.resource)
			}
			actions.RequireMutations(t)
			resources := cl.Resource(test.resource)
			ns := ""
			if schema.Namespaced {
				ns = actions.CreateTestNamespace(t, cl)
				resources = resources.Namespace(ns)
			} else {
				actions.RequireClusterScopedCRDs(t)
			}
			caseID := actions.UniqueName(t, "indexed-field")
			lowFilter := test.field + "=" + strconv.Quote(test.lowValue)
			highFilter := test.field + "=" + strconv.Quote(test.highValue)
			names := []string{"z-low", "a-high"}
			// Reverse alphabetical names catch a sort that silently uses name instead
			// of the requested field. 2 and 10 also distinguish numeric and text order.
			for i, body := range []map[string]any{test.low, test.high} {
				metadata, _ := body["metadata"].(map[string]any)
				if metadata == nil {
					metadata = make(map[string]any)
				}
				if name, ok := metadata["name"].(string); ok {
					names[i] = name // CRD names must be plural.group.
				}
				metadata["name"] = names[i]
				if ns != "" {
					metadata["namespace"] = ns
				}
				labels, _ := metadata["labels"].(map[string]string)
				labels = actions.ManagedLabels(labels)
				labels[actions.CaseLabel] = caseID
				metadata["labels"] = labels
				body["metadata"] = metadata
				actions.CreateAndWait(t, resources, names[i], body)
			}
			check := func(t *testing.T, filter, sort string, want []string) {
				t.Helper()
				values := url.Values{
					"filter": {"metadata.labels." + actions.CaseLabel + "=" + caseID},
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
				{"filter low", lowFilter, "metadata.name", []string{names[0]}},
				{"filter high", highFilter, "metadata.name", []string{names[1]}},
				{"ascending", "", test.field + ",metadata.name", []string{names[0], names[1]}},
				{"descending", "", "-" + test.field + ",metadata.name", []string{names[1], names[0]}},
			} {
				t.Run(assertion.name, func(t *testing.T) {
					check(t, assertion.filter, assertion.sort, assertion.want)
				})
			}
			if test.update == nil {
				t.Run("delete refreshes index", func(t *testing.T) {
					check(t, lowFilter, "metadata.name", []string{names[0]})
					if err := resources.DeleteContext(t.Context(), names[0]); err != nil {
						t.Fatalf("delete indexed resource: %v", err)
					}
					check(t, lowFilter, "metadata.name", []string{})
					check(t, "", test.field, []string{names[1]})
				})
				return
			}
			// This subtest performs its own transition and does not rely on siblings.
			t.Run("update refreshes index", func(t *testing.T) {
				check(t, lowFilter, "metadata.name", []string{names[0]})
				if _, err := actions.UpdateWithConflictRetry(t.Context(), resources, names[0], test.update); err != nil {
					t.Fatalf("update indexed field: %v", err)
				}
				check(t, highFilter, "metadata.name", []string{names[1], names[0]})
				check(t, lowFilter, "metadata.name", []string{})
				check(t, "", test.field+",metadata.name", []string{names[1], names[0]})
				if test.remove != nil {
					t.Run("remove annotation refreshes index", func(t *testing.T) {
						if _, err := actions.UpdateWithConflictRetry(t.Context(), resources, names[0], test.remove); err != nil {
							t.Fatalf("remove indexed field: %v", err)
						}
						check(t, highFilter, "metadata.name", []string{names[1]})
						check(t, lowFilter, "metadata.name", []string{})
						// A resource without this annotation must remain in the collection.
						check(t, "", "metadata.name", []string{names[1], names[0]})
					})
				}
			})
		})
	}
}

// A nil value removes the field.
func updateField(value any, path ...string) func(map[string]any) error {
	return func(current map[string]any) error {
		for _, key := range path[:len(path)-1] {
			next, ok := current[key].(map[string]any)
			if !ok {
				return fmt.Errorf("resource has no %s object in %v", key, path)
			}
			current = next
		}
		key := path[len(path)-1]
		if value == nil {
			delete(current, key)
		} else {
			current[key] = value
		}
		return nil
	}
}
