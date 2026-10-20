package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

// Secret metadata.fields[1] supports filtering and sorting on all targets.
// Check the _type query alias where supported. Diagnostics omit Secret bodies.
func TestSecretTypeFilterAndSort(t *testing.T) {
	t.Parallel()
	setup := actions.RequireSQLMode(t)
	namespace := actions.CreateTestNamespace(t, setup.Client)
	resources := setup.Client.Resource("secret").Namespace(namespace)
	caseID := actions.UniqueName(t, "secret-type")

	fixtures := []struct {
		name       string
		secretType string
		stringData map[string]string
	}{
		{
			name:       "type-basic-auth",
			secretType: "kubernetes.io/basic-auth",
			stringData: map[string]string{"username": "steve-test", "password": "not-a-real-password"},
		},
		{
			name:       "type-opaque",
			secretType: "Opaque",
			stringData: map[string]string{"example": "opaque-value"},
		},
	}
	for _, fixture := range fixtures {
		body := map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      fixture.name,
				"namespace": namespace,
				"labels":    actions.ManagedLabels(map[string]string{actions.CaseLabel: caseID}),
			},
			"type":       fixture.secretType,
			"stringData": fixture.stringData,
		}
		actions.CreateAndWait(t, resources, fixture.name, body)
	}

	caseFilter := query.Equal("metadata.labels."+actions.CaseLabel, caseID)
	for _, test := range []struct {
		name          string
		filter        string
		wantNames     []string
		requiresAlias bool
	}{
		{name: "synthetic type filter", filter: query.Equal("_type", "Opaque"), wantNames: []string{"type-opaque"}, requiresAlias: true},
		{name: "table field type filter", filter: query.Equal("metadata.fields[1]", "Opaque"), wantNames: []string{"type-opaque"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.requiresAlias {
				actions.RequireSteveFeature(t, actions.SteveSecretTypeAlias)
			}
			values := url.Values{query.FilterParam: {caseFilter, test.filter}}
			collection := waitForSecretTypeCollection(t, resources, values, test.wantNames)
			assertSecretTypes(t, collection, map[string]string{"type-opaque": "Opaque"})
		})
	}

	wantOrder := append([]struct {
		name       string
		secretType string
		stringData map[string]string
	}(nil), fixtures...)
	caseInsensitive := actions.SteveSupports(actions.SteveCaseInsensitiveSort)
	sort.Slice(wantOrder, func(i, j int) bool {
		left := wantOrder[i].secretType
		right := wantOrder[j].secretType
		if caseInsensitive {
			left = strings.ToLower(left)
			right = strings.ToLower(right)
		}
		if left == right {
			return wantOrder[i].name < wantOrder[j].name
		}
		return left < right
	})
	expectedNames := make([]string, 0, len(wantOrder))
	expectedTypes := make(map[string]string, len(wantOrder))
	for _, fixture := range wantOrder {
		expectedNames = append(expectedNames, fixture.name)
		expectedTypes[fixture.name] = fixture.secretType
	}
	for _, field := range []string{"_type", "metadata.fields[1]"} {
		t.Run("sort "+field, func(t *testing.T) {
			if field == "_type" {
				actions.RequireSteveFeature(t, actions.SteveSecretTypeAlias)
			}
			values := url.Values{
				query.FilterParam: {caseFilter},
				query.SortParam:   {field + ",metadata.name"},
			}
			collection := waitForSecretTypeCollection(t, resources, values, expectedNames)
			assertSecretTypes(t, collection, expectedTypes)
		})
	}
}

func waitForSecretTypeCollection(t *testing.T, resources *client.ResourceClient, values url.Values, expectedNames []string) *client.SteveCollection {
	t.Helper()
	timeout := resources.WaitTimeout()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	interval := resources.PollInterval()
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	var lastNames []string
	for {
		collection, err := resources.ListValuesContext(ctx, values)
		if err == nil {
			lastNames = secretCollectionNames(collection)
			if equalSecretNames(lastNames, expectedNames) {
				return collection
			}
		} else if !client.IsStatus(err, 408, 425, 429, 500, 502, 503, 504) {
			t.Fatalf("Secret type query failed: %s", secretMetadataError(err))
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("timed out waiting for Secret type query names %v; last names=%v", expectedNames, lastNames)
		case <-timer.C:
		}
	}
}

func secretCollectionNames(collection *client.SteveCollection) []string {
	if collection == nil {
		return nil
	}
	names := make([]string, 0, len(collection.Data))
	for _, item := range collection.Data {
		metadata, _ := item["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		names = append(names, name)
	}
	return names
}

func assertSecretTypes(t *testing.T, collection *client.SteveCollection, expected map[string]string) {
	t.Helper()
	if collection == nil {
		t.Fatal("Secret collection is nil")
	}
	for _, item := range collection.Data {
		metadata, _ := item["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		secretType, _ := item["_type"].(string)
		want, ok := expected[name]
		if !ok || secretType != want {
			t.Fatalf("Secret %q _type = %q, want %q", name, secretType, want)
		}
		fields, _ := metadata["fields"].([]any)
		if len(fields) <= 1 || fields[1] != want {
			t.Fatalf("Secret %q metadata.fields[1] = %v, want type %q", name, fieldAt(fields, 1), want)
		}
	}
}

func fieldAt(fields []any, index int) any {
	if index < 0 || index >= len(fields) {
		return nil
	}
	return fields[index]
}

func equalSecretNames(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func secretMetadataError(err error) string {
	var statusErr *client.StatusError
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("%s %s status=%d", statusErr.Method, statusErr.Path, statusErr.StatusCode)
	}
	return err.Error()
}
