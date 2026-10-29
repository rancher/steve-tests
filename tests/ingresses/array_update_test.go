package ingresses

import (
	"fmt"
	"net/url"
	"slices"
	"testing"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/query"
)

// Array indexes must discard old membership and positions when rules change.
// A fixed control Ingress also makes stale sort values observable.
func TestIngressArrayIndexesRefreshAfterUpdate(t *testing.T) {
	t.Parallel()
	actions.RequireSteveFeature(t, actions.SteveArrayElementIndexes)
	cl := requireIngressSQLTarget(t)

	const targetName, controlName = "array-target", "array-control"
	tests := []struct {
		name       string
		afterHosts []string
		firstOrder []string
	}{
		{
			name:       "replace hosts",
			afterHosts: []string{"zulu.example.test", "beta.example.test"},
			firstOrder: []string{controlName, targetName},
		},
		{
			name:       "reorder hosts",
			afterHosts: []string{"zulu.example.test", "alpha.example.test"},
			firstOrder: []string{controlName, targetName},
		},
		{
			name:       "shorten rules",
			afterHosts: []string{"alpha.example.test"},
			firstOrder: []string{targetName, controlName},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			namespace := actions.CreateTestNamespace(t, cl)
			resources := cl.Resource(ingressSchemaID).Namespace(namespace)
			caseID := actions.UniqueName(t, "ingress-array-update")
			beforeHosts := []string{"alpha.example.test", "zulu.example.test"}
			actions.CreateAndWait(t, resources, targetName, newIngressBody(namespace, targetName, caseID, beforeHosts))
			actions.CreateAndWait(t, resources, controlName, newIngressBody(namespace, controlName, caseID, []string{"mike.example.test", "mike.example.test"}))

			caseFilter := query.Equal("metadata.labels."+actions.CaseLabel, caseID)
			candidates := append(slices.Clone(beforeHosts), test.afterHosts...)
			slices.Sort(candidates)
			candidates = slices.Compact(candidates)
			checkIndexes := func(t *testing.T, hosts, firstOrder []string) {
				t.Helper()
				t.Run("membership", func(t *testing.T) {
					actions.RequireSteveFeature(t, actions.SteveArrayMembershipFilters)
					for _, host := range candidates {
						t.Run(host, func(t *testing.T) {
							var expected []string
							if slices.Contains(hosts, host) {
								expected = []string{targetName}
							}
							values := url.Values{
								query.FilterParam: {caseFilter, query.Contains("spec.rules.host", host)},
								query.SortParam:   {"metadata.name"},
							}
							waitForIngressOrder(t, resources, values, expected)
						})
					}
				})
				for index := range beforeHosts {
					field := fmt.Sprintf("spec.rules.%d.host", index)
					for _, host := range candidates {
						t.Run(field+"="+host, func(t *testing.T) {
							var expected []string
							if index < len(hosts) && hosts[index] == host {
								expected = []string{targetName}
							}
							values := url.Values{
								query.FilterParam: {caseFilter, query.Equal(field, host)},
								query.SortParam:   {"metadata.name"},
							}
							waitForIngressOrder(t, resources, values, expected)
						})
					}
				}
				t.Run("first host sort", func(t *testing.T) {
					values := url.Values{
						query.FilterParam: {caseFilter},
						query.SortParam:   {"spec.rules.0.host,metadata.name"},
					}
					waitForIngressOrder(t, resources, values, firstOrder)
				})
			}

			// Establish all initial indexes before updating, so an empty old-host
			// result after the update cannot pass because indexing never happened.
			if !t.Run("before update", func(t *testing.T) {
				checkIndexes(t, beforeHosts, []string{targetName, controlName})
			}) {
				return
			}
			if _, err := actions.UpdateWithConflictRetry(t.Context(), resources, targetName, func(current map[string]any) error {
				spec, ok := current["spec"].(map[string]any)
				if !ok {
					return fmt.Errorf("Ingress has no spec object")
				}
				replacement := newIngressBody(namespace, targetName, caseID, test.afterHosts)
				spec["rules"] = replacement["spec"].(map[string]any)["rules"]
				return nil
			}); err != nil {
				t.Fatalf("update Ingress rules: %v", err)
			}
			t.Run("after update", func(t *testing.T) {
				checkIndexes(t, test.afterHosts, test.firstOrder)
			})
		})
	}
}
