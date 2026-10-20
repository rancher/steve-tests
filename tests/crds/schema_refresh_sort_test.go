// Package crds contains deliberately cluster-scoped regression tests. Every
// test in this package must pass both the SQL and CRD mutation gates before it
// creates anything.
package crds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/config"
)

const (
	crdAPIVersion = "apiextensions.k8s.io/v1"
	crdKind       = "CustomResourceDefinition"
	probeVersion  = "v1"
	probeKind     = "SteveSortProbe"
	probePlural   = "stevesortprobes"
)

type dynamicSchemaCollection struct {
	Data []dynamicSchema `json:"data"`
}

type dynamicSchema struct {
	ID         string            `json:"id"`
	Links      map[string]string `json:"links"`
	Attributes struct {
		Group      string          `json:"group"`
		Version    string          `json:"version"`
		Kind       string          `json:"kind"`
		Resource   string          `json:"resource"`
		Namespaced bool            `json:"namespaced"`
		Columns    []dynamicColumn `json:"columns"`
	} `json:"attributes"`
}

type dynamicColumn struct {
	Name  string `json:"name"`
	Field string `json:"field"`
	Type  string `json:"type"`
}

type dynamicSchemaSelector struct {
	Group      string
	Version    string
	Kind       string
	Resource   string
	Namespaced bool
}

type probeFixture struct {
	Name             string
	SortValue        string
	ObservedAt       string
	TailValue        string
	FinalizedAt      string
	ReplacementValue string
}

// Check generated names, date-column alignment, and indexed sorting while
// adding, replacing, and removing printer columns on an isolated CRD.
func TestCRDPrinterColumnRefreshAndCaseInsensitiveSort(t *testing.T) {
	// Both probes are read-only. Keep them before CreateTestNamespace and every
	// other mutation so an accidental partial opt-in cannot touch the cluster.
	sqlSetup := actions.RequireSQLMode(t)
	crdSetup := actions.RequireClusterScopedCRDs(t)
	if sqlSetup.Client != crdSetup.Client {
		t.Fatal("integration capability gates returned different Steve clients")
	}
	setup := crdSetup

	namespace := actions.CreateTestNamespace(t, setup.Client)
	groupPrefix := actions.UniqueName(t, "refresh")
	group := groupPrefix + ".steve-tests.invalid"
	crdName := probePlural + "." + group

	crdSchema, ok := findClientSchemaByGVK(setup.Client, crdAPIVersion, crdKind)
	if !ok {
		t.Fatalf("Steve does not expose the %s %s schema required by the enabled CRD profile", crdAPIVersion, crdKind)
	}
	crds := setup.Client.Resource(crdSchema.ID)

	// Register before the POST. If the API server persists the CRD but the
	// response is lost, cleanup still runs; Delete treats a missing CRD as an
	// already-clean state.
	registerStrictCRDCleanup(t, crds, crdName, setup.Config.WaitTimeoutDuration())
	if _, err := crds.Create(newProbeCRD(crdName, group, groupPrefix)); err != nil {
		t.Fatalf("create isolated CRD %q: %v", crdName, metadataOnlyError(err))
	}

	selector := dynamicSchemaSelector{
		Group:      group,
		Version:    probeVersion,
		Kind:       probeKind,
		Resource:   probePlural,
		Namespaced: true,
	}
	discovered := waitForDynamicSchema(t, setup.Client, selector, setup.Config.WaitTimeoutDuration(), setup.Config.PollIntervalDuration())

	// Refresh the client from /v1/schemas after the dynamic API is registered.
	// The schema ID comes from the response; no group-to-ID convention is baked
	// into this test.
	dynamicClient := waitForDiscoveredSchemaClient(
		t,
		setup.Config,
		discovered.ID,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)
	probes := dynamicClient.Resource(discovered.ID).Namespace(namespace)
	waitForDynamicCollectionReady(
		t,
		probes,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)

	for _, test := range []struct{ name, prefix string }{
		{name: "default generated name prefix", prefix: "ssp-"},
	} {
		t.Run(test.name, func(t *testing.T) {
			created, err := probes.Create(newGeneratedProbeResource(group, namespace, groupPrefix))
			if err != nil {
				t.Fatalf("create dynamic resource without metadata.name or metadata.generateName: %s", metadataOnlyError(err))
			}
			name, err := nestedString(created, "metadata", "name")
			if err != nil {
				t.Fatalf("generated resource response: %v", err)
			}
			t.Cleanup(func() {
				if err := probes.Delete(name); err != nil {
					t.Errorf("clean up generated probe %q: %s", name, metadataOnlyError(err))
					return
				}
				if err := probes.WaitForDelete(name, setup.Config.WaitTimeoutDuration()); err != nil {
					t.Errorf("generated probe %q did not disappear: %s", name, metadataOnlyError(err))
				}
			})

			// Steve follows Norman's Kind-initial convention for multi-word Kinds:
			// SteveSortProbe -> ssp-. The old bug used the first character of the
			// group-qualified schema ID instead.
			wantPrefix := test.prefix
			if !actions.SteveSupports(actions.SteveKindNamePrefix) {
				wantPrefix = discovered.ID[:1] + "-"
				t.Logf("Checking legacy schema-derived generated name prefix %q", wantPrefix)
			}
			if !strings.HasPrefix(name, wantPrefix) {
				t.Fatalf("generated name = %q, want prefix %q (schema ID %q)", name, wantPrefix, discovered.ID)
			}
		})
	}

	fixtures := []probeFixture{
		{Name: "sort-alpha", SortValue: "alpha", ObservedAt: "2025-01-02T03:04:05Z", TailValue: "tail-a", FinalizedAt: "2023-01-02T03:04:05Z", ReplacementValue: "Zulu"},
		{Name: "sort-bravo", SortValue: "Bravo", ObservedAt: "2024-01-02T03:04:05Z", TailValue: "tail-b", FinalizedAt: "2022-01-02T03:04:05Z", ReplacementValue: "alpha"},
		{Name: "sort-charlie", SortValue: "charlie", ObservedAt: "2026-01-02T03:04:05Z", TailValue: "tail-c", FinalizedAt: "2024-01-02T03:04:05Z", ReplacementValue: "Mike"},
	}
	observedAtMarkers := fixtureObservedAtMarkers(fixtures)
	finalizedAtMarkers := fixtureFinalizedAtMarkers(fixtures)
	if !actions.SteveSupports(actions.SteveRawDateColumns) {
		// Older Steve releases keep Kubernetes' human-readable Table cells.
		// Ages between eight days and two years print as whole days; center
		// these fixtures within a day so their exact cells stay stable for
		// twelve hours, regardless of the time this test starts.
		now := time.Now().UTC()
		for index, ages := range []struct{ observed, finalized int }{{20, 40}, {30, 50}, {10, 30}} {
			fixtures[index].ObservedAt = now.Add(-time.Duration(ages.observed)*24*time.Hour - 12*time.Hour).Format(time.RFC3339)
			fixtures[index].FinalizedAt = now.Add(-time.Duration(ages.finalized)*24*time.Hour - 12*time.Hour).Format(time.RFC3339)
			observedAtMarkers[fixtures[index].Name] = fmt.Sprintf("%dd", ages.observed)
			finalizedAtMarkers[fixtures[index].Name] = fmt.Sprintf("%dd", ages.finalized)
		}
		t.Log("Checking legacy human-readable date cells")
	}
	for _, fixture := range fixtures {
		body := newProbeResource(group, namespace, groupPrefix, fixture)
		actions.CreateAndWait(t, probes, fixture.Name, body)
	}

	initialFields := waitForPrinterFieldState(
		t,
		setup.Client,
		probes,
		selector,
		fixtures,
		false,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)
	t.Logf("initial dynamic resource metadata: names=%v fields=%v", fixtureNames(fixtures), initialFields)

	if err := updateCRDPrinterColumns(
		t.Context(),
		crds,
		crdName,
		initialProbeColumns(),
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	); err != nil {
		t.Fatalf("add string/date/string/date printer columns to CRD %q: %v", crdName, metadataOnlyError(err))
	}

	updatedFields := waitForPrinterFieldState(
		t,
		setup.Client,
		probes,
		selector,
		fixtures,
		true,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
		observedAtMarkers,
		fixtureTailMarkers(fixtures),
		finalizedAtMarkers,
	)
	fieldIndex, err := commonMarkerIndex(updatedFields, fixtureMarkers(fixtures))
	if err != nil {
		t.Fatalf("updated metadata.fields did not expose one stable printer-column index: %v", err)
	}
	dateIndex, err := commonMarkerIndex(updatedFields, observedAtMarkers)
	if err != nil {
		t.Fatalf("updated metadata.fields did not preserve the expected date-column values: %v", err)
	}
	tailIndex, err := commonMarkerIndex(updatedFields, fixtureTailMarkers(fixtures))
	if err != nil {
		t.Fatalf("updated metadata.fields did not expose the trailing column values: %v", err)
	}
	finalDateIndex, err := commonMarkerIndex(updatedFields, finalizedAtMarkers)
	if err != nil {
		t.Fatalf("updated metadata.fields did not preserve the expected final date-column values: %v", err)
	}
	if !(fieldIndex < dateIndex && dateIndex < tailIndex && tailIndex < finalDateIndex) {
		t.Fatalf("printer-column indexes = sort:%d date:%d tail:%d finalDate:%d, want configured string/date/string/date order", fieldIndex, dateIndex, tailIndex, finalDateIndex)
	}
	waitForDynamicSchemaColumns(
		t,
		setup.Client,
		selector,
		map[string]expectedDynamicColumn{
			"Name":         {Index: 0, Type: "string", Absent: !actions.SteveSupports(actions.StevePrinterColumnFieldMapping)},
			"Sort Value":   expectedPrinterColumn(fieldIndex, "string", ".spec.sortValue"),
			"Observed At":  expectedPrinterColumn(dateIndex, "date", ".spec.observedAt"),
			"Tail Value":   expectedPrinterColumn(tailIndex, "string", ".spec.tailValue"),
			"Finalized At": expectedPrinterColumn(finalDateIndex, "date", ".spec.finalizedAt"),
			"Age":          {Absent: true},
		},
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)
	t.Logf("refreshed dynamic resource metadata: names=%v sortField=metadata.fields[%d] dateField=metadata.fields[%d] tailField=metadata.fields[%d] finalDateField=metadata.fields[%d]", fixtureNames(fixtures), fieldIndex, dateIndex, tailIndex, finalDateIndex)

	tests := []struct {
		name        string
		sort        string
		filter      string
		order       []string
		legacyOrder []string
	}{
		{
			name:        "ascending string column",
			sort:        fmt.Sprintf("metadata.fields[%d]", fieldIndex),
			order:       []string{"sort-alpha", "sort-bravo", "sort-charlie"},
			legacyOrder: []string{"sort-bravo", "sort-alpha", "sort-charlie"},
		},
		{
			name:        "descending string column",
			sort:        fmt.Sprintf("-metadata.fields[%d]", fieldIndex),
			order:       []string{"sort-charlie", "sort-bravo", "sort-alpha"},
			legacyOrder: []string{"sort-charlie", "sort-alpha", "sort-bravo"},
		},
		{
			name:  "date column ascending",
			sort:  fmt.Sprintf("metadata.fields[%d]", dateIndex),
			order: []string{"sort-bravo", "sort-alpha", "sort-charlie"},
		},
		{
			name:  "date column descending",
			sort:  fmt.Sprintf("-metadata.fields[%d]", dateIndex),
			order: []string{"sort-charlie", "sort-alpha", "sort-bravo"},
		},
		{name: "printer field filter", filter: fmt.Sprintf("metadata.fields[%d]=Bravo", fieldIndex), order: []string{"sort-bravo"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actions.RequireSteveFeature(t, actions.StevePrinterColumnFieldMapping)
			values := make(url.Values)
			if test.sort != "" {
				values.Set("sort", test.sort)
			}
			if test.filter != "" {
				values.Set("filter", test.filter)
			}
			order := test.order
			if test.legacyOrder != nil && !actions.SteveSupports(actions.SteveCaseInsensitiveSort) {
				order = test.legacyOrder
				t.Log("Checking legacy case-sensitive string ordering")
			}
			waitForExactOrder(
				t,
				probes,
				values.Encode(),
				order,
				setup.Config.WaitTimeoutDuration(),
				setup.Config.PollIntervalDuration(),
			)
		})
	}

	// Replace an existing printer column with a different non-empty layout.
	// ReplacementValue is deliberately absent from the first Table layout, so
	// observing it proves Steve rebuilt this type's SQL column mapping instead
	// of continuing to serve the previous cache schema.
	if err := updateCRDPrinterColumns(
		t.Context(),
		crds,
		crdName,
		replacementProbeColumns(),
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	); err != nil {
		t.Fatalf("replace non-empty printer columns on CRD %q: %v", crdName, metadataOnlyError(err))
	}
	replacementFixtures := make([]probeFixture, 0, len(fixtures))
	for _, fixture := range fixtures {
		replacementFixtures = append(replacementFixtures, probeFixture{Name: fixture.Name, SortValue: fixture.ReplacementValue})
	}
	replacementFields := waitForPrinterFieldState(
		t,
		setup.Client,
		probes,
		selector,
		replacementFixtures,
		true,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)
	replacementIndex, err := commonMarkerIndex(replacementFields, fixtureReplacementMarkers(fixtures))
	if err != nil {
		t.Fatalf("replacement column did not appear after non-empty CRD column update: %v", err)
	}
	waitForDynamicSchemaColumns(
		t,
		setup.Client,
		selector,
		map[string]expectedDynamicColumn{
			"Name":              {Index: 0, Type: "string", Absent: !actions.SteveSupports(actions.StevePrinterColumnFieldMapping)},
			"Replacement Value": expectedPrinterColumn(replacementIndex, "string", ".spec.replacementValue"),
			"Age":               {Absent: true},
			"Sort Value":        {Absent: true},
			"Observed At":       {Absent: true},
			"Tail Value":        {Absent: true},
			"Finalized At":      {Absent: true},
		},
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)
	for _, test := range []struct {
		name        string
		sort        string
		filter      string
		order       []string
		legacyOrder []string
	}{
		{name: "replacement ascending", sort: fmt.Sprintf("metadata.fields[%d]", replacementIndex),
			order: []string{"sort-bravo", "sort-charlie", "sort-alpha"}, legacyOrder: []string{"sort-charlie", "sort-alpha", "sort-bravo"}},
		{name: "replacement descending", sort: fmt.Sprintf("-metadata.fields[%d]", replacementIndex),
			order: []string{"sort-alpha", "sort-charlie", "sort-bravo"}, legacyOrder: []string{"sort-bravo", "sort-alpha", "sort-charlie"}},
		{name: "replacement filter", filter: fmt.Sprintf("metadata.fields[%d]=Mike", replacementIndex), order: []string{"sort-charlie"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			actions.RequireSteveFeature(t, actions.StevePrinterColumnFieldMapping)
			values := make(url.Values)
			if test.sort != "" {
				values.Set("sort", test.sort)
			}
			if test.filter != "" {
				values.Set("filter", test.filter)
			}
			order := test.order
			if test.legacyOrder != nil && !actions.SteveSupports(actions.SteveCaseInsensitiveSort) {
				order = test.legacyOrder
				t.Log("Checking legacy case-sensitive replacement ordering")
			}
			waitForExactOrder(t, probes, values.Encode(), order, setup.Config.WaitTimeoutDuration(), setup.Config.PollIntervalDuration())
		})
	}

	// Remove all additional printer columns and expect the default Name/Age
	// fields. The previous index 1 now refers to Age, so check field names too.
	if err := removeCRDPrinterColumns(
		t.Context(),
		crds,
		crdName,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	); err != nil {
		t.Fatalf("remove all printer columns from CRD %q: %v", crdName, metadataOnlyError(err))
	}
	waitForDefaultPrinterFields(
		t,
		probes,
		fixtures,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)
	waitForOnlyDefaultDynamicSchemaColumns(
		t,
		setup.Client,
		selector,
		setup.Config.WaitTimeoutDuration(),
		setup.Config.PollIntervalDuration(),
	)
}

func newProbeCRD(name string, group string, caseID string) map[string]any {
	return map[string]any{
		"apiVersion": crdAPIVersion,
		"kind":       crdKind,
		"metadata": map[string]any{
			"name":   name,
			"labels": actions.ManagedLabels(map[string]string{actions.CaseLabel: caseID}),
		},
		"spec": map[string]any{
			"group": group,
			"names": map[string]any{
				"kind":     probeKind,
				"listKind": probeKind + "List",
				"plural":   probePlural,
				"singular": "stevesortprobe",
			},
			"scope": "Namespaced",
			"versions": []any{
				map[string]any{
					"name":    probeVersion,
					"served":  true,
					"storage": true,
					"schema": map[string]any{
						"openAPIV3Schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"spec": map[string]any{
									"type":     "object",
									"required": []any{"sortValue", "observedAt", "tailValue", "finalizedAt", "replacementValue"},
									"properties": map[string]any{
										"sortValue":        map[string]any{"type": "string"},
										"observedAt":       map[string]any{"type": "string", "format": "date-time"},
										"tailValue":        map[string]any{"type": "string"},
										"finalizedAt":      map[string]any{"type": "string", "format": "date-time"},
										"replacementValue": map[string]any{"type": "string"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func newProbeResource(group string, namespace string, caseID string, fixture probeFixture) map[string]any {
	return map[string]any{
		"apiVersion": group + "/" + probeVersion,
		"kind":       probeKind,
		"metadata": map[string]any{
			"name":      fixture.Name,
			"namespace": namespace,
			"labels":    actions.ManagedLabels(map[string]string{actions.CaseLabel: caseID}),
		},
		"spec": map[string]any{
			"sortValue":        fixture.SortValue,
			"observedAt":       fixture.ObservedAt,
			"tailValue":        fixture.TailValue,
			"finalizedAt":      fixture.FinalizedAt,
			"replacementValue": fixture.ReplacementValue,
		},
	}
}

func newGeneratedProbeResource(group string, namespace string, caseID string) map[string]any {
	return map[string]any{
		"apiVersion": group + "/" + probeVersion,
		"kind":       probeKind,
		"metadata": map[string]any{
			"namespace": namespace,
			"labels":    actions.ManagedLabels(map[string]string{actions.CaseLabel: caseID}),
		},
		"spec": map[string]any{
			"sortValue":        "generated",
			"observedAt":       "2025-01-02T03:04:05Z",
			"tailValue":        "generated-tail",
			"finalizedAt":      "2026-01-02T03:04:05Z",
			"replacementValue": "generated-replacement",
		},
	}
}

func findClientSchemaByGVK(cl *client.Client, apiVersion string, kind string) (client.SchemaInfo, bool) {
	if cl == nil {
		return client.SchemaInfo{}, false
	}
	for _, schema := range cl.Schemas {
		if schema.APIVersion == apiVersion && schema.Kind == kind {
			return schema, true
		}
	}
	return client.SchemaInfo{}, false
}

func fetchDynamicSchema(ctx context.Context, cl *client.Client, selector dynamicSchemaSelector) (dynamicSchema, bool, error) {
	resp, body, err := cl.GetContext(ctx, "v1/schemas")
	if err != nil {
		return dynamicSchema{}, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return dynamicSchema{}, false, fmt.Errorf("GET v1/schemas returned status %d", resp.StatusCode)
	}

	var collection dynamicSchemaCollection
	if err := json.Unmarshal(body, &collection); err != nil {
		return dynamicSchema{}, false, fmt.Errorf("decode v1/schemas response: %w", err)
	}
	return selectDynamicSchema(collection.Data, selector)
}

func selectDynamicSchema(schemas []dynamicSchema, selector dynamicSchemaSelector) (dynamicSchema, bool, error) {
	var matches []dynamicSchema
	for _, schema := range schemas {
		attributes := schema.Attributes
		if attributes.Group == selector.Group &&
			attributes.Version == selector.Version &&
			attributes.Kind == selector.Kind &&
			attributes.Resource == selector.Resource &&
			attributes.Namespaced == selector.Namespaced {
			matches = append(matches, schema)
		}
	}
	if len(matches) == 0 {
		return dynamicSchema{}, false, nil
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		sort.Strings(ids)
		return dynamicSchema{}, false, fmt.Errorf("dynamic schema selector matched multiple IDs: %v", ids)
	}
	if strings.TrimSpace(matches[0].ID) == "" {
		return dynamicSchema{}, false, errors.New("dynamic schema matched by attributes has an empty ID")
	}
	if strings.TrimSpace(matches[0].Links["collection"]) == "" {
		return dynamicSchema{}, false, fmt.Errorf("dynamic schema %q has no collection link", matches[0].ID)
	}
	return matches[0], true, nil
}

func waitForDynamicSchema(t *testing.T, cl *client.Client, selector dynamicSchemaSelector, timeout time.Duration, interval time.Duration) dynamicSchema {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	var lastState string
	for {
		schema, found, err := fetchDynamicSchema(ctx, cl, selector)
		switch {
		case err == nil && found:
			return schema
		case err != nil:
			lastState = metadataOnlyError(err)
		default:
			lastState = fmt.Sprintf("schema for group=%q version=%q kind=%q resource=%q not registered", selector.Group, selector.Version, selector.Kind, selector.Resource)
		}

		if !waitForNextPoll(ctx, interval) {
			t.Fatalf("timed out waiting for dynamic Steve schema discovery: %s", lastState)
		}
	}
}

func waitForDiscoveredSchemaClient(
	t *testing.T,
	cfg *config.Config,
	schemaID string,
	timeout time.Duration,
	interval time.Duration,
) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	lastState := "no discovery response"
	for {
		discoveredClient, err := client.DiscoverContext(ctx, cfg)
		if err != nil {
			lastState = metadataOnlyError(err)
		} else if _, ok := discoveredClient.Schemas[schemaID]; ok {
			return discoveredClient
		} else {
			lastState = fmt.Sprintf("schema %q absent from discovery response", schemaID)
			discoveredClient.HTTPClient.CloseIdleConnections()
		}
		if !waitForNextPoll(ctx, interval) {
			t.Fatalf("timed out refreshing Steve discovery for dynamic schema %q: %s", schemaID, lastState)
		}
	}
}

func registerStrictCRDCleanup(t *testing.T, crds *client.ResourceClient, name string, timeout time.Duration) {
	t.Helper()
	t.Cleanup(func() {
		t.Logf("Cleaning up cluster-scoped CRD %q...", name)
		ctx, cancel := context.WithTimeout(context.Background(), positiveDuration(timeout, 30*time.Second))
		defer cancel()
		if err := crds.DeleteContext(ctx, name); err != nil {
			t.Errorf("failed to delete cluster-scoped CRD %q; manual cleanup is required: %s", name, metadataOnlyError(err))
			return
		}
		if err := crds.WaitForDeleteContext(ctx, name, positiveDuration(timeout, 30*time.Second)); err != nil {
			t.Errorf("cluster-scoped CRD %q did not disappear; manual cleanup is required: %s", name, metadataOnlyError(err))
		}
	})
}

// waitForDynamicCollectionReady closes the small gap between schema
// registration and SQL cache initialization. Steve's known cache-warmup 500s
// and a transient 404 from a lagging HA replica are retried; authorization and
// other server failures remain immediate test failures.
func waitForDynamicCollectionReady(t *testing.T, resources *client.ResourceClient, timeout time.Duration, interval time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	attempts := 0
	for {
		attempts++
		_, err := resources.ListContext(ctx, "pagesize=1")
		if err == nil {
			return
		}
		if !isSQLCacheWarmup(err) && !client.IsStatus(err, http.StatusNotFound) {
			t.Fatalf("dynamic collection failed its readiness probe: %s", metadataOnlyError(err))
		}
		if !waitForNextPoll(ctx, interval) {
			t.Fatalf("timed out waiting for dynamic SQL collection readiness after %d attempts; last response: %s", attempts, metadataOnlyError(err))
		}
	}
}

func isSQLCacheWarmup(err error) bool {
	var statusErr *client.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
		return false
	}
	body := strings.ToLower(statusErr.Body)
	return strings.Contains(body, "sql cache sync") || strings.Contains(body, "cache context canceled")
}

func updateCRDPrinterColumns(ctx context.Context, crds *client.ResourceClient, name string, columns []any, timeout time.Duration, interval time.Duration) error {
	return mutateCRDWithRetry(ctx, crds, name, timeout, interval, func(current map[string]any) error {
		return setPrinterColumns(current, columns)
	})
}

func removeCRDPrinterColumns(ctx context.Context, crds *client.ResourceClient, name string, timeout time.Duration, interval time.Duration) error {
	return mutateCRDWithRetry(ctx, crds, name, timeout, interval, unsetPrinterColumns)
}

func mutateCRDWithRetry(
	ctx context.Context,
	crds *client.ResourceClient,
	name string,
	timeout time.Duration,
	interval time.Duration,
	mutate func(map[string]any) error,
) error {
	if mutate == nil {
		return errors.New("CRD mutator is nil")
	}
	ctx, cancel := context.WithTimeout(ctx, positiveDuration(timeout, 30*time.Second))
	defer cancel()

	for {
		current, err := crds.GetContext(ctx, name)
		if err == nil {
			err = mutate(current)
		}
		if err == nil {
			_, err = crds.UpdateContext(ctx, name, current)
		}
		if err == nil {
			return nil
		}
		if !client.IsStatus(err, http.StatusConflict, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable) {
			return err
		}
		if !waitForNextPoll(ctx, interval) {
			return fmt.Errorf("timed out updating CRD %q; last response: %s", name, metadataOnlyError(err))
		}
	}
}

func unsetPrinterColumns(crd map[string]any) error {
	versions, err := nestedSlice(crd, "spec", "versions")
	if err != nil {
		return err
	}
	for _, rawVersion := range versions {
		version, ok := rawVersion.(map[string]any)
		if !ok {
			return fmt.Errorf("spec.versions contains %T, want object", rawVersion)
		}
		if version["name"] != probeVersion {
			continue
		}
		delete(version, "additionalPrinterColumns")
		return nil
	}
	return fmt.Errorf("spec.versions does not contain %q", probeVersion)
}

func setPrinterColumns(crd map[string]any, columns []any) error {
	versions, err := nestedSlice(crd, "spec", "versions")
	if err != nil {
		return err
	}
	for _, rawVersion := range versions {
		version, ok := rawVersion.(map[string]any)
		if !ok {
			return fmt.Errorf("spec.versions contains %T, want object", rawVersion)
		}
		if version["name"] != probeVersion {
			continue
		}
		version["additionalPrinterColumns"] = columns
		return nil
	}
	return fmt.Errorf("spec.versions does not contain %q", probeVersion)
}

func initialProbeColumns() []any {
	return []any{
		map[string]any{
			"name":     "Sort Value",
			"type":     "string",
			"jsonPath": ".spec.sortValue",
		},
		map[string]any{
			"name":     "Observed At",
			"type":     "date",
			"jsonPath": ".spec.observedAt",
		},
		map[string]any{
			"name":     "Tail Value",
			"type":     "string",
			"jsonPath": ".spec.tailValue",
		},
		map[string]any{
			"name":     "Finalized At",
			"type":     "date",
			"jsonPath": ".spec.finalizedAt",
		},
	}
}

func replacementProbeColumns() []any {
	return []any{
		map[string]any{
			"name":     "Replacement Value",
			"type":     "string",
			"jsonPath": ".spec.replacementValue",
		},
	}
}

type expectedDynamicColumn struct {
	Index  int
	Field  string
	Type   string
	Absent bool
}

func expectedPrinterColumn(index int, columnType, legacyPath string) expectedDynamicColumn {
	expected := expectedDynamicColumn{Index: index, Type: columnType}
	if !actions.SteveSupports(actions.StevePrinterColumnFieldMapping) {
		expected.Field = legacyPath
	}
	return expected
}

func waitForDynamicSchemaColumns(
	t *testing.T,
	cl *client.Client,
	selector dynamicSchemaSelector,
	expected map[string]expectedDynamicColumn,
	timeout time.Duration,
	interval time.Duration,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	var lastState string
	for {
		schema, found, err := fetchDynamicSchema(ctx, cl, selector)
		switch {
		case err != nil:
			lastState = metadataOnlyError(err)
		case !found:
			lastState = "dynamic schema temporarily absent"
		default:
			if err := verifyDynamicSchemaColumns(schema.Attributes.Columns, expected); err == nil {
				return
			} else {
				lastState = err.Error()
			}
		}
		if !waitForNextPoll(ctx, interval) {
			t.Fatalf("timed out waiting for dynamic schema columns: %s", lastState)
		}
	}
}

func waitForOnlyDefaultDynamicSchemaColumns(
	t *testing.T,
	cl *client.Client,
	selector dynamicSchemaSelector,
	timeout time.Duration,
	interval time.Duration,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	var lastState string
	for {
		schema, found, err := fetchDynamicSchema(ctx, cl, selector)
		switch {
		case err != nil:
			lastState = metadataOnlyError(err)
		case !found:
			lastState = "dynamic schema temporarily absent"
		default:
			expected := map[string]expectedDynamicColumn{
				"Name":              {Index: 0, Type: "string"},
				"Age":               {Index: 1, Type: "date"},
				"Replacement Value": {Absent: true},
				"Sort Value":        {Absent: true},
				"Observed At":       {Absent: true},
				"Tail Value":        {Absent: true},
				"Finalized At":      {Absent: true},
			}
			if verifyErr := verifyDynamicSchemaColumns(schema.Attributes.Columns, expected); verifyErr != nil {
				lastState = verifyErr.Error()
			} else if len(schema.Attributes.Columns) != 2 {
				lastState = fmt.Sprintf("schema columns = %v, want exactly [Name Age]", columnNames(schema.Attributes.Columns))
			} else {
				return
			}
		}
		if !waitForNextPoll(ctx, interval) {
			t.Fatalf("timed out waiting for only default dynamic schema columns: %s", lastState)
		}
	}
}

func verifyDynamicSchemaColumns(columns []dynamicColumn, expected map[string]expectedDynamicColumn) error {
	byName := make(map[string]dynamicColumn, len(columns))
	for _, column := range columns {
		if _, duplicate := byName[column.Name]; duplicate {
			return fmt.Errorf("schema columns contain duplicate name %q", column.Name)
		}
		byName[column.Name] = column
	}
	for name, want := range expected {
		column, found := byName[name]
		if want.Absent {
			if found {
				return fmt.Errorf("schema still exposes removed column %q as %#v", name, column)
			}
			continue
		}
		if !found {
			return fmt.Errorf("schema columns %v do not contain %q", columnNames(columns), name)
		}
		wantField := fmt.Sprintf("$.metadata.fields[%d]", want.Index)
		if want.Field != "" {
			wantField = want.Field
		}
		if column.Field != wantField || column.Type != want.Type {
			return fmt.Errorf("schema column %q = field:%q type:%q, want field:%q type:%q", name, column.Field, column.Type, wantField, want.Type)
		}
	}
	return nil
}

func columnNames(columns []dynamicColumn) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.Name)
	}
	sort.Strings(names)
	return names
}

func waitForPrinterFieldState(
	t *testing.T,
	schemaClient *client.Client,
	resources *client.ResourceClient,
	selector dynamicSchemaSelector,
	fixtures []probeFixture,
	wantPresent bool,
	timeout time.Duration,
	interval time.Duration,
	additionalMarkers ...map[string]string,
) map[string][]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	var lastFields map[string][]any
	var lastState string
	for {
		_, found, schemaErr := fetchDynamicSchema(ctx, schemaClient, selector)
		if schemaErr == nil && found {
			collection, listErr := resources.ListContext(ctx, "sort=metadata.name")
			if listErr == nil {
				fields, parseErr := metadataFieldsByName(collection)
				if parseErr == nil {
					lastFields = fields
					if stateErr := verifyMarkerState(fields, fixtures, wantPresent, additionalMarkers...); stateErr == nil {
						return fields
					} else {
						lastState = stateErr.Error()
					}
				} else {
					lastState = parseErr.Error()
				}
			} else {
				lastState = "list response: " + metadataOnlyError(listErr)
			}
		} else if schemaErr != nil {
			lastState = "schema response: " + metadataOnlyError(schemaErr)
		} else {
			lastState = "dynamic schema temporarily absent"
		}

		if !waitForNextPoll(ctx, interval) {
			phase := "appear in"
			if !wantPresent {
				phase = "be absent from"
			}
			t.Fatalf("timed out waiting for printer values to %s metadata.fields; last state: %s; metadata.fields=%v", phase, lastState, lastFields)
		}
	}
}

func waitForDefaultPrinterFields(
	t *testing.T,
	resources *client.ResourceClient,
	fixtures []probeFixture,
	timeout time.Duration,
	interval time.Duration,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	var lastState string
	for {
		collection, err := resources.ListContext(ctx, "sort=metadata.name")
		if err != nil {
			lastState = metadataOnlyError(err)
		} else if fields, parseErr := metadataFieldsByName(collection); parseErr != nil {
			lastState = parseErr.Error()
		} else if len(fields) != len(fixtures) {
			lastState = fmt.Sprintf("metadata.fields has %d resources, want %d", len(fields), len(fixtures))
		} else {
			valid := true
			for _, fixture := range fixtures {
				values, ok := fields[fixture.Name]
				if !ok {
					lastState = fmt.Sprintf("metadata.fields missing resource %q", fixture.Name)
					valid = false
					break
				}
				if len(values) != 2 || values[0] != fixture.Name {
					lastState = fmt.Sprintf("metadata.fields for %q = %v, want [name age]", fixture.Name, values)
					valid = false
					break
				}
				if dateErr := verifyPrinterDateCell(values[1], actions.SteveSupports(actions.SteveRawDateColumns)); dateErr != nil {
					lastState = fmt.Sprintf("metadata.fields age for %q: %v", fixture.Name, dateErr)
					valid = false
					break
				}
			}
			if valid {
				return
			}
		}
		if !waitForNextPoll(ctx, interval) {
			t.Fatalf("timed out waiting for default Name/Age metadata.fields: %s", lastState)
		}
	}
}

// Kubernetes Table date cells use a compact age before Steve's raw-date
// conversion. Accept only that duration grammar on historical targets.
var humanReadableAge = regexp.MustCompile(`^[0-9]+(?:s|m(?:[0-9]+s)?|h(?:[0-9]+m)?|d(?:[0-9]+h)?|y(?:[0-9]+d)?)$`)

func verifyPrinterDateCell(value any, rawDate bool) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("date cell has type %T, want string", value)
	}
	if rawDate {
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			return fmt.Errorf("date cell %q is not an RFC3339 timestamp: %w", text, err)
		}
		return nil
	}
	if !humanReadableAge.MatchString(text) {
		return fmt.Errorf("date cell %q is not a human-readable Kubernetes age", text)
	}
	return nil
}

func waitForExactOrder(t *testing.T, resources *client.ResourceClient, query string, expected []string, timeout time.Duration, interval time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	var actual []string
	var lastState string
	for {
		collection, err := resources.ListContext(ctx, query)
		if err == nil {
			actual, err = collectionNames(collection)
			if err == nil && equalStrings(actual, expected) {
				return
			}
		}
		if err != nil {
			lastState = metadataOnlyError(err)
		} else {
			lastState = fmt.Sprintf("names=%v", actual)
		}
		if !waitForNextPoll(ctx, interval) {
			t.Fatalf("timed out waiting for exact order %v; last state: %s", expected, lastState)
		}
	}
}

func metadataFieldsByName(collection *client.SteveCollection) (map[string][]any, error) {
	if collection == nil {
		return nil, errors.New("collection is nil")
	}
	result := make(map[string][]any, len(collection.Data))
	for index, item := range collection.Data {
		metadata, ok := item["metadata"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d has no metadata object", index)
		}
		name, ok := metadata["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("item %d has no metadata.name", index)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("collection contains duplicate metadata.name %q", name)
		}
		fields, ok := metadata["fields"].([]any)
		if !ok {
			return nil, fmt.Errorf("item %q has no metadata.fields array", name)
		}
		result[name] = append([]any(nil), fields...)
	}
	return result, nil
}

func verifyMarkerState(fields map[string][]any, fixtures []probeFixture, wantPresent bool, additionalMarkers ...map[string]string) error {
	if len(fields) != len(fixtures) {
		return fmt.Errorf("got metadata.fields for %d resources, want %d", len(fields), len(fixtures))
	}
	for _, fixture := range fixtures {
		resourceFields, ok := fields[fixture.Name]
		if !ok {
			return fmt.Errorf("metadata.fields missing resource %q", fixture.Name)
		}
		present := fieldIndex(resourceFields, fixture.SortValue) >= 0
		if present != wantPresent {
			return fmt.Errorf("resource %q marker %q presence=%t, want %t", fixture.Name, fixture.SortValue, present, wantPresent)
		}
	}
	// Column refresh can expose ordinary Table strings before the date
	// transformer has caught up. Require the complete requested layout so
	// observing one string does not prematurely declare refresh complete.
	for _, markers := range additionalMarkers {
		if _, err := commonMarkerIndex(fields, markers); err != nil {
			return err
		}
	}
	return nil
}

func commonMarkerIndex(fields map[string][]any, markers map[string]string) (int, error) {
	common := -1
	for name, marker := range markers {
		resourceFields, ok := fields[name]
		if !ok {
			return -1, fmt.Errorf("metadata.fields missing resource %q", name)
		}
		index := fieldIndex(resourceFields, marker)
		if index < 0 {
			return -1, fmt.Errorf("metadata.fields for %q does not contain marker %q", name, marker)
		}
		if common < 0 {
			common = index
			continue
		}
		if index != common {
			return -1, fmt.Errorf("marker %q for %q is at index %d, want common index %d", marker, name, index, common)
		}
	}
	if common < 0 {
		return -1, errors.New("no markers supplied")
	}
	return common, nil
}

func fieldIndex(fields []any, marker string) int {
	for index, field := range fields {
		if value, ok := field.(string); ok && value == marker {
			return index
		}
	}
	return -1
}

func collectionNames(collection *client.SteveCollection) ([]string, error) {
	if collection == nil {
		return nil, errors.New("collection is nil")
	}
	names := make([]string, 0, len(collection.Data))
	for index, item := range collection.Data {
		metadata, ok := item["metadata"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d has no metadata object", index)
		}
		name, ok := metadata["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("item %d has no metadata.name", index)
		}
		names = append(names, name)
	}
	return names, nil
}

func nestedSlice(value map[string]any, keys ...string) ([]any, error) {
	var current any = value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an object", strings.Join(keys, "."))
		}
		current = object[key]
	}
	result, ok := current.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an array", strings.Join(keys, "."))
	}
	return result, nil
}

func nestedString(value map[string]any, keys ...string) (string, error) {
	var current any = value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s is not an object", strings.Join(keys, "."))
		}
		current = object[key]
	}
	result, ok := current.(string)
	if !ok || strings.TrimSpace(result) == "" {
		return "", fmt.Errorf("%s is not a non-empty string", strings.Join(keys, "."))
	}
	return result, nil
}

func fixtureNames(fixtures []probeFixture) []string {
	result := make([]string, 0, len(fixtures))
	for _, fixture := range fixtures {
		result = append(result, fixture.Name)
	}
	return result
}

func fixtureMarkers(fixtures []probeFixture) map[string]string {
	result := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		result[fixture.Name] = fixture.SortValue
	}
	return result
}

func fixtureObservedAtMarkers(fixtures []probeFixture) map[string]string {
	result := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		result[fixture.Name] = fixture.ObservedAt
	}
	return result
}

func fixtureTailMarkers(fixtures []probeFixture) map[string]string {
	result := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		result[fixture.Name] = fixture.TailValue
	}
	return result
}

func fixtureFinalizedAtMarkers(fixtures []probeFixture) map[string]string {
	result := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		result[fixture.Name] = fixture.FinalizedAt
	}
	return result
}

func fixtureReplacementMarkers(fixtures []probeFixture) map[string]string {
	result := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		result[fixture.Name] = fixture.ReplacementValue
	}
	return result
}

func equalStrings(left []string, right []string) bool {
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

func positiveDuration(value time.Duration, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func waitForNextPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(positiveDuration(interval, 200*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// metadataOnlyError deliberately omits HTTP response bodies because dynamic
// objects can contain arbitrary user-shaped fields. Status, method, and path
// are enough to diagnose this lifecycle test.
func metadataOnlyError(err error) string {
	if err == nil {
		return "none"
	}
	var statusErr *client.StatusError
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("%s %s status=%d", statusErr.Method, statusErr.Path, statusErr.StatusCode)
	}
	return err.Error()
}
