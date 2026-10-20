// Package ingresses covers Steve SQL indexing behavior for Kubernetes Ingress
// array fields. The fixtures use an explicit, uniquely nonexistent
// IngressClass and reference nonexistent Services, so controllers ignore them
// and they never create workloads or external load balancers.
package ingresses

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

const ingressSchemaID = "networking.k8s.io.ingress"

// An Ingress with an omitted host must remain visible through GET and list.
func TestHostlessIngressRemainsVisible(t *testing.T) {
	t.Parallel()
	actions.RequireSteveFeature(t, actions.SteveIngressMissingHost)
	tests := []struct {
		name  string
		hosts []string
	}{
		{name: "hostless rule", hosts: []string{""}},
		{name: "hostless and named rules", hosts: []string{"", "example.test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cl := requireIngressSQLTarget(t)
			namespace := actions.CreateTestNamespace(t, cl)
			resources := cl.Resource(ingressSchemaID).Namespace(namespace)
			name := actions.UniqueName(t, "hostless-ingress")
			caseID := actions.UniqueName(t, "hostless-ingress-case")

			actions.CreateAndWait(t, resources, name, newIngressBody(namespace, name, caseID, test.hosts))
			if _, err := resources.GetContext(t.Context(), name); err != nil {
				t.Fatalf("GET hostless Ingress %q after collection convergence: %v", name, err)
			}
			values := url.Values{query.FilterParam: {query.Equal("metadata.labels."+actions.CaseLabel, caseID)}}
			waitForIngressOrder(t, resources, values, []string{name})
		})
	}
}

// Check exact array membership and indexed host queries using Steve's dotted array indexes.
func TestIngressArrayFieldExactFilterAndIndexedSort(t *testing.T) {
	t.Parallel()
	actions.RequireSteveFeature(t, actions.SteveArrayElementIndexes)
	cl := requireIngressSQLTarget(t)
	namespace := actions.CreateTestNamespace(t, cl)
	resources := cl.Resource(ingressSchemaID).Namespace(namespace)
	caseID := actions.UniqueName(t, "ingress-array")

	fixtures := []struct {
		name  string
		hosts []string
	}{
		{name: "array-a", hosts: []string{"zulu.example.test", "alpha.example.test"}},
		{name: "array-b", hosts: []string{"alpha.example.test", "zulu.example.test"}},
		{name: "array-c", hosts: []string{"alpha.example.test.evil", "mike.example.test"}},
	}
	for _, fixture := range fixtures {
		actions.CreateAndWait(t, resources, fixture.name, newIngressBody(namespace, fixture.name, caseID, fixture.hosts))
	}

	caseFilter := query.Equal("metadata.labels."+actions.CaseLabel, caseID)
	for _, test := range []struct {
		name     string
		filters  []string
		sort     string
		expected []string
		feature  actions.SteveFeature
	}{
		{
			name:     "exact contains matches array members but not substrings",
			filters:  []string{caseFilter, query.Contains("spec.rules.host", "alpha.example.test")},
			sort:     "metadata.name",
			expected: []string{"array-a", "array-b"},
			feature:  actions.SteveArrayMembershipFilters,
		},
		{
			name:     "first host dotted exact filter",
			filters:  []string{caseFilter, query.Equal("spec.rules.0.host", "alpha.example.test")},
			sort:     "metadata.name",
			expected: []string{"array-b"},
		},
		{
			name:     "second host dotted exact filter",
			filters:  []string{caseFilter, query.Equal("spec.rules.1.host", "alpha.example.test")},
			sort:     "metadata.name",
			expected: []string{"array-a"},
		},
		{
			name:     "first host dotted sort",
			filters:  []string{caseFilter},
			sort:     "spec.rules.0.host,metadata.name",
			expected: []string{"array-b", "array-c", "array-a"},
		},
		{
			name:     "second host dotted sort",
			filters:  []string{caseFilter},
			sort:     "spec.rules.1.host,metadata.name",
			expected: []string{"array-a", "array-c", "array-b"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.feature != "" {
				actions.RequireSteveFeature(t, test.feature)
			}
			values := make(url.Values)
			for _, filter := range test.filters {
				values.Add(query.FilterParam, filter)
			}
			values.Set(query.SortParam, test.sort)
			waitForIngressOrder(t, resources, values, test.expected)
		})
	}
}

func requireIngressSQLTarget(t *testing.T) *client.Client {
	t.Helper()
	setup := actions.RequireSQLMode(t)
	if _, ok := setup.Client.Schema(ingressSchemaID); !ok {
		t.Skipf("target does not expose schema %q", ingressSchemaID)
	}
	return setup.Client
}

func newIngressBody(namespace string, name string, caseID string, hosts []string) map[string]any {
	rules := make([]any, 0, len(hosts))
	for _, host := range hosts {
		rule := map[string]any{
			"http": map[string]any{
				"paths": []any{
					map[string]any{
						"path":     "/",
						"pathType": "Prefix",
						"backend": map[string]any{
							"service": map[string]any{
								"name": "steve-test-nonexistent",
								"port": map[string]any{"number": 80},
							},
						},
					},
				},
			},
		}
		if host != "" {
			rule["host"] = host
		}
		rules = append(rules, rule)
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    actions.ManagedLabels(map[string]string{actions.CaseLabel: caseID}),
		},
		"spec": map[string]any{
			"ingressClassName": caseID,
			"rules":            rules,
		},
	}
}

func waitForIngressOrder(t *testing.T, resources *client.ResourceClient, values url.Values, expected []string) {
	t.Helper()
	timeout := resources.WaitTimeout()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	interval := resources.PollInterval()
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	var lastNames []string
	var lastErr error
	for {
		collection, err := resources.ListValuesContext(ctx, values)
		if err == nil {
			lastNames, err = ingressCollectionNames(collection)
			if err != nil {
				t.Fatalf("parse Ingress collection: %v", err)
			}
			if equalIngressNames(lastNames, expected) {
				return
			}
		} else if !isRetryableIngressQueryError(err) {
			t.Fatalf("Ingress query failed: %v", err)
		}
		lastErr = err

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("timed out waiting for Ingress collection names %v; last names=%v last error=%v", expected, lastNames, lastErr)
		case <-timer.C:
		}
	}
}

func ingressCollectionNames(collection *client.SteveCollection) ([]string, error) {
	if collection == nil {
		return nil, errors.New("Ingress collection is nil")
	}
	actual := make([]string, 0, len(collection.Data))
	for index, item := range collection.Data {
		metadata, ok := item["metadata"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Ingress collection item %d has no metadata object", index)
		}
		name, _ := metadata["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("Ingress collection item %d has no metadata.name", index)
		}
		actual = append(actual, name)
	}
	return actual, nil
}

func equalIngressNames(actual []string, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range expected {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func isRetryableIngressQueryError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *client.StatusError
	if !errors.As(err, &statusErr) {
		return true
	}
	if statusErr == nil {
		return false
	}
	return statusErr.StatusCode == http.StatusRequestTimeout ||
		statusErr.StatusCode == http.StatusTooEarly ||
		statusErr.StatusCode == http.StatusTooManyRequests ||
		statusErr.StatusCode >= http.StatusInternalServerError
}
