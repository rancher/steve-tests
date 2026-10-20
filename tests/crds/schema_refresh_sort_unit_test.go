package crds

import (
	"strings"
	"testing"

	"github.com/rancher/steve-tests/pkg/client"
)

func TestSelectDynamicSchema(t *testing.T) {
	selector := dynamicSchemaSelector{
		Group:      "run-a.steve-tests.invalid",
		Version:    "v1",
		Kind:       probeKind,
		Resource:   probePlural,
		Namespaced: true,
	}
	matching := dynamicSchema{ID: "opaque.schema.id", Links: map[string]string{"collection": "/v1/opaque.schema.id"}}
	matching.Attributes.Group = selector.Group
	matching.Attributes.Version = selector.Version
	matching.Attributes.Kind = selector.Kind
	matching.Attributes.Resource = selector.Resource
	matching.Attributes.Namespaced = true

	tests := []struct {
		name      string
		schemas   []dynamicSchema
		wantFound bool
		wantID    string
		wantErr   string
	}{
		{
			name:      "matches attributes without deriving ID",
			schemas:   []dynamicSchema{{ID: selector.Group + ".wrong"}, matching},
			wantFound: true,
			wantID:    "opaque.schema.id",
		},
		{
			name:    "not registered",
			schemas: []dynamicSchema{{ID: "unrelated"}},
		},
		{
			name:    "ambiguous metadata is rejected",
			schemas: []dynamicSchema{matching, matching},
			wantErr: "multiple IDs",
		},
		{
			name: "empty ID is rejected",
			schemas: func() []dynamicSchema {
				value := matching
				value.ID = ""
				return []dynamicSchema{value}
			}(),
			wantErr: "empty ID",
		},
		{
			name: "missing collection link is rejected",
			schemas: func() []dynamicSchema {
				value := matching
				value.Links = nil
				return []dynamicSchema{value}
			}(),
			wantErr: "no collection link",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found, err := selectDynamicSchema(test.schemas, selector)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("selectDynamicSchema() error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectDynamicSchema() error: %v", err)
			}
			if found != test.wantFound {
				t.Fatalf("selectDynamicSchema() found = %t, want %t", found, test.wantFound)
			}
			if got.ID != test.wantID {
				t.Fatalf("selectDynamicSchema() ID = %q, want %q", got.ID, test.wantID)
			}
		})
	}
}

func TestIsSQLCacheWarmup(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "cache sync 500",
			err:  &client.StatusError{StatusCode: 500, Body: "waiting for SQL cache sync"},
			want: true,
		},
		{
			name: "cache context 500",
			err:  &client.StatusError{StatusCode: 500, Body: "cache context canceled"},
			want: true,
		},
		{
			name: "unrelated 500",
			err:  &client.StatusError{StatusCode: 500, Body: "database is corrupt"},
		},
		{
			name: "same text wrong status",
			err:  &client.StatusError{StatusCode: 422, Body: "SQL cache sync"},
		},
		{name: "nil"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isSQLCacheWarmup(test.err); got != test.want {
				t.Fatalf("isSQLCacheWarmup() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestPrinterFieldHelpers(t *testing.T) {
	t.Run("date cells", testVerifyPrinterDateCell)
	t.Run("refresh waits for all printer columns", func(t *testing.T) {
		fixtures := []probeFixture{{Name: "sort-alpha", SortValue: "alpha"}}
		fields := map[string][]any{
			"sort-alpha": {"sort-alpha", "alpha", "20d", "tail-a", "2023-01-02T03:04:05Z"},
		}
		markers := []map[string]string{
			{"sort-alpha": "2025-01-02T03:04:05Z"},
			{"sort-alpha": "tail-a"},
			{"sort-alpha": "2023-01-02T03:04:05Z"},
		}
		if err := verifyMarkerState(fields, fixtures, true); err != nil {
			t.Fatalf("string printer marker should already be visible: %v", err)
		}
		if err := verifyMarkerState(fields, fixtures, true, markers...); err == nil {
			t.Fatal("refresh completed while the date cell still held a stale human duration")
		}
		fields["sort-alpha"][2] = "2025-01-02T03:04:05Z"
		if err := verifyMarkerState(fields, fixtures, true, markers...); err != nil {
			t.Fatalf("complete refreshed layout was rejected: %v", err)
		}
		fields["sort-alpha"][3] = ""
		if err := verifyMarkerState(fields, fixtures, true, markers...); err == nil {
			t.Fatal("refresh completed without the trailing string column")
		}
	})
	fixtures := []probeFixture{{Name: "sort-alpha", SortValue: "alpha"}, {Name: "sort-bravo", SortValue: "Bravo"}}
	tests := []struct {
		name      string
		fields    map[string][]any
		present   bool
		wantIndex int
		wantErr   string
	}{
		{name: "initial markers absent", fields: map[string][]any{
			"sort-alpha": {"sort-alpha", "2026-08-03T00:00:00Z"},
			"sort-bravo": {"sort-bravo", "2026-08-03T00:00:00Z"},
		}},
		{name: "updated markers share one index", present: true, wantIndex: 2, fields: map[string][]any{
			"sort-alpha": {"sort-alpha", "unused", "alpha"}, "sort-bravo": {"sort-bravo", "unused", "Bravo"},
		}},
		{name: "different indexes rejected", present: true, wantErr: "common index", fields: map[string][]any{
			"sort-alpha": {"alpha", "unused"}, "sort-bravo": {"unused", "Bravo"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := verifyMarkerState(test.fields, fixtures, test.present); err != nil {
				t.Fatal(err)
			}
			if !test.present {
				return
			}
			index, err := commonMarkerIndex(test.fields, fixtureMarkers(fixtures))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("commonMarkerIndex() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if index != test.wantIndex {
				t.Fatalf("index = %d, want %d", index, test.wantIndex)
			}
		})
	}
}

func TestMetadataFieldsByName(t *testing.T) {
	tests := []struct {
		name    string
		input   *client.SteveCollection
		want    map[string][]any
		wantErr string
	}{
		{
			name: "parses metadata only",
			input: &client.SteveCollection{Data: []map[string]any{
				{
					"metadata": map[string]any{"name": "alpha", "fields": []any{"alpha", "value"}},
					"spec":     map[string]any{"sortValue": "must-not-be-read"},
				},
			}},
			want: map[string][]any{"alpha": {"alpha", "value"}},
		},
		{
			name:    "nil collection",
			wantErr: "collection is nil",
		},
		{
			name: "missing fields",
			input: &client.SteveCollection{Data: []map[string]any{
				{"metadata": map[string]any{"name": "alpha"}},
			}},
			wantErr: "no metadata.fields array",
		},
		{
			name: "duplicate names",
			input: &client.SteveCollection{Data: []map[string]any{
				{"metadata": map[string]any{"name": "alpha", "fields": []any{}}},
				{"metadata": map[string]any{"name": "alpha", "fields": []any{}}},
			}},
			wantErr: "duplicate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := metadataFieldsByName(test.input)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("metadataFieldsByName() error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("metadataFieldsByName() error: %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("metadataFieldsByName() result = %v, want %v", got, test.want)
			}
			for name, wantFields := range test.want {
				gotFields := got[name]
				if !equalAny(gotFields, wantFields) {
					t.Fatalf("metadataFieldsByName()[%q] = %v, want %v", name, gotFields, wantFields)
				}
			}
		})
	}
}

func TestSetPrinterColumns(t *testing.T) {
	tests := []struct {
		name                 string
		replace, remove      bool
		wantPaths, wantTypes []string
	}{
		{name: "initial columns", wantPaths: []string{".spec.sortValue", ".spec.observedAt", ".spec.tailValue", ".spec.finalizedAt"}, wantTypes: []string{"string", "date", "string", "date"}},
		{name: "replace existing columns", replace: true, wantPaths: []string{".spec.replacementValue"}, wantTypes: []string{"string"}},
		{name: "remove replacement columns", replace: true, remove: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			crd := newProbeCRD("stevesortprobes.example.test", "example.test", "unit-case")
			if err := setPrinterColumns(crd, initialProbeColumns()); err != nil {
				t.Fatal(err)
			}
			if test.replace {
				if err := setPrinterColumns(crd, replacementProbeColumns()); err != nil {
					t.Fatal(err)
				}
			}
			if test.remove {
				if err := unsetPrinterColumns(crd); err != nil {
					t.Fatal(err)
				}
			}
			versions, err := nestedSlice(crd, "spec", "versions")
			if err != nil {
				t.Fatal(err)
			}
			version := versions[0].(map[string]any)
			if test.remove {
				if _, present := version["additionalPrinterColumns"]; present {
					t.Fatal("printer columns remain after unset")
				}
				return
			}
			columns, ok := version["additionalPrinterColumns"].([]any)
			if !ok || len(columns) != len(test.wantPaths) {
				t.Fatalf("columns = %#v, want %d", version["additionalPrinterColumns"], len(test.wantPaths))
			}
			for i, raw := range columns {
				column := raw.(map[string]any)
				if column["jsonPath"] != test.wantPaths[i] || column["type"] != test.wantTypes[i] {
					t.Fatalf("column %d = %#v, want %s %s", i, column, test.wantTypes[i], test.wantPaths[i])
				}
			}
		})
	}
}

func TestVerifyDynamicSchemaColumns(t *testing.T) {
	t.Run("legacy JSONPath columns omit implicit name and age", func(t *testing.T) {
		columns := []dynamicColumn{
			{Name: "Sort Value", Field: ".spec.sortValue", Type: "string"},
			{Name: "Observed At", Field: ".spec.observedAt", Type: "date"},
		}
		expected := map[string]expectedDynamicColumn{
			"Name":        {Absent: true},
			"Age":         {Absent: true},
			"Sort Value":  {Field: ".spec.sortValue", Type: "string"},
			"Observed At": {Field: ".spec.observedAt", Type: "date"},
		}
		if err := verifyDynamicSchemaColumns(columns, expected); err != nil {
			t.Fatal(err)
		}
		columns[1].Field = "$.metadata.fields[2]"
		if err := verifyDynamicSchemaColumns(columns, expected); err == nil {
			t.Fatal("legacy JSONPath expectation accepted a different field mapping")
		}
	})

	columns := []dynamicColumn{
		{Name: "Name", Field: "$.metadata.fields[0]", Type: "string"},
		{Name: "Observed At", Field: "$.metadata.fields[2]", Type: "date"},
	}
	if err := verifyDynamicSchemaColumns(columns, map[string]expectedDynamicColumn{
		"Name":        {Index: 0, Type: "string"},
		"Observed At": {Index: 2, Type: "date"},
		"Removed":     {Absent: true},
	}); err != nil {
		t.Fatalf("verifyDynamicSchemaColumns() error: %v", err)
	}

	for _, test := range []struct {
		name    string
		expect  map[string]expectedDynamicColumn
		wantErr string
	}{
		{name: "missing", expect: map[string]expectedDynamicColumn{"Tail": {Index: 3, Type: "string"}}, wantErr: "do not contain"},
		{name: "wrong index", expect: map[string]expectedDynamicColumn{"Name": {Index: 1, Type: "string"}}, wantErr: "want field"},
		{name: "removed remains", expect: map[string]expectedDynamicColumn{"Observed At": {Absent: true}}, wantErr: "still exposes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyDynamicSchemaColumns(columns, test.expect)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verifyDynamicSchemaColumns() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func testVerifyPrinterDateCell(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		rawDate bool
		wantErr string
	}{
		{name: "raw timestamp", value: "2026-01-02T03:04:05Z", rawDate: true},
		{name: "raw timestamp with offset", value: "2026-01-02T03:04:05-04:00", rawDate: true},
		{name: "legacy seconds", value: "0s"},
		{name: "legacy minutes and seconds", value: "3m20s"},
		{name: "legacy hours", value: "25h"},
		{name: "legacy whole days", value: "30d"},
		{name: "legacy years and days", value: "2y30d"},
		{name: "legacy age on modern target", value: "30d", rawDate: true, wantErr: "not an RFC3339 timestamp"},
		{name: "raw timestamp on legacy target", value: "2026-01-02T03:04:05Z", wantErr: "not a human-readable Kubernetes age"},
		{name: "empty age", value: "", wantErr: "not a human-readable Kubernetes age"},
		{name: "invalid age", value: "<invalid>", wantErr: "not a human-readable Kubernetes age"},
		{name: "non-date column", value: "tail-a", wantErr: "not a human-readable Kubernetes age"},
		{name: "numeric cell", value: 30, wantErr: "want string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyPrinterDateCell(test.value, test.rawDate)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verifyPrinterDateCell() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func equalAny(left []any, right []any) bool {
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
