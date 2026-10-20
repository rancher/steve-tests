package configmaps

import (
	"fmt"
	"testing"

	"github.com/rancher/steve-tests/actions"
)

// TestConfigMapsNumericFieldSortAndCompare verifies sorting and range filters
// for the ConfigMap DATA printer column (metadata.fields[1], the number of data
// keys). Releases with the integer index compare numerically; older releases
// retain coverage of their original text index.
func TestConfigMapsNumericFieldSortAndCompare(t *testing.T) {
	t.Parallel()
	cl := actions.RequireSQLMode(t).Client
	ns := actions.CreateTestNamespace(t, cl)
	resources := cl.Resource("configmap").Namespace(ns)
	caseID := actions.UniqueName(t, "configmap-numeric")
	labels := map[string]string{actions.CaseLabel: caseID}

	seeds := []struct {
		name string
		keys int
	}{
		{name: "keys-one", keys: 1},
		{name: "keys-two", keys: 2},
		{name: "keys-ten", keys: 10},
	}
	for _, seed := range seeds {
		data := map[string]string{}
		for i := 0; i < seed.keys; i++ {
			data[fmt.Sprintf("key-%02d", i)] = "v"
		}
		body := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      seed.name,
				"namespace": ns,
				"labels":    actions.ManagedLabels(labels),
			},
			"data": data,
		}
		actions.CreateAndWait(t, resources, seed.name, body)
	}

	baseFilter := fmt.Sprintf("filter=metadata.labels.%s=%s", actions.CaseLabel, caseID)

	ascending := []string{"keys-one", "keys-two", "keys-ten"}
	descending := []string{"keys-ten", "keys-two", "keys-one"}
	lessThanThree := []string{"keys-one", "keys-two"}
	if !actions.SteveSupports(actions.SteveNumericConfigMapCount) {
		ascending = []string{"keys-one", "keys-ten", "keys-two"}
		descending = []string{"keys-two", "keys-ten", "keys-one"}
		lessThanThree = []string{"keys-one", "keys-ten", "keys-two"}
		t.Log("Checking ConfigMap DATA count against this release's text index")
	}

	tests := []struct {
		name, query string
		want        []string
	}{
		{"ascending sort", "&sort=metadata.fields[1]", ascending},
		{"descending sort", "&sort=-metadata.fields[1]", descending},
		{"greater-than filter", "&filter=metadata.fields[1]%3E1&sort=metadata.name", []string{"keys-ten", "keys-two"}},
		{"less-than filter", "&filter=metadata.fields[1]%3C3&sort=metadata.name", lessThanThree},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actions.WaitForCollection(t, resources, baseFilter+test.query, actions.CollectionExpectation{Order: test.want})
		})
	}
}
