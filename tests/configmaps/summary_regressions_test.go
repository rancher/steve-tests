package configmaps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

type summaryResponse struct {
	Data    []map[string]any `json:"data"`
	Summary []summaryItem    `json:"summary"`
}

type summaryItem struct {
	Property string                  `json:"property"`
	Counts   map[string]summaryCount `json:"counts"`
}

type summaryCount struct {
	Total     int            `json:"total"`
	Namespace map[string]int `json:"namespace,omitempty"`
}

// Versions with SQL summaries must preserve totals. Versions that support
// summary options must also preserve per-namespace counts and omit data.
func TestConfigMapSummaryOnlyAndNamespaced(t *testing.T) {
	t.Parallel()
	actions.RequireSteveFeature(t, actions.SteveBasicSummary)
	cl := actions.RequireSQLMode(t).Client
	firstNS := actions.CreateTestNamespace(t, cl)
	secondNS := actions.CreateTestNamespace(t, cl)
	caseID := actions.UniqueName(t, "summary")

	seeds := []struct {
		namespace string
		name      string
		bucket    string
	}{
		{namespace: firstNS, name: "summary-blue", bucket: "blue"},
		{namespace: firstNS, name: "summary-green", bucket: "green"},
		{namespace: secondNS, name: "summary-blue", bucket: "blue"},
	}
	for _, seed := range seeds {
		labels := map[string]string{
			actions.CaseLabel: caseID,
			"summary-bucket":  seed.bucket,
		}
		resources := cl.Resource("configmap").Namespace(seed.namespace)
		actions.CreateAndWait(t, resources, seed.name, newConfigMapBody(seed.namespace, seed.name, labels))
	}

	summaryField := "metadata.labels[summary-bucket]"
	base := url.Values{
		query.FilterParam:  {query.Equal("metadata.labels."+actions.CaseLabel, caseID)},
		query.SummaryParam: {summaryField},
	}
	wantTotals := []summaryItem{{
		Property: summaryField,
		Counts: map[string]summaryCount{
			"blue":  {Total: 2},
			"green": {Total: 1},
		},
	}}
	if !t.Run("summary preserves totals", func(t *testing.T) {
		structured := actions.SteveSupports(actions.SteveStructuredSummary)
		deadline := time.Now().Add(cl.WaitTimeout)
		for {
			status, body := requestConfigMapSummary(t, cl, base)
			if status != http.StatusOK {
				t.Fatalf("SQL summary query returned status %d: %s", status, compactBody(body))
			}
			probe, err := decodeSummaryResponse(body, structured)
			if err != nil {
				t.Fatalf("decode SQL summary response (structured=%t): %v", structured, err)
			}
			if len(probe.Data) == 3 && reflect.DeepEqual(probe.Summary, wantTotals) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("data length = %d, summary = %#v; want 3 resources and exactly %#v", len(probe.Data), probe.Summary, wantTotals)
			}
			time.Sleep(cl.PollInterval)
		}
	}) {
		return
	}

	wantSummary := []summaryItem{
		{
			Property: summaryField,
			Counts: map[string]summaryCount{
				"blue": {
					Total:     2,
					Namespace: map[string]int{firstNS: 1, secondNS: 1},
				},
				"green": {
					Total:     1,
					Namespace: map[string]int{firstNS: 1},
				},
			},
		},
	}

	tests := []struct {
		name           string
		summaryOnly    *string
		includeSummary bool
		wantStatus     int
		wantData       int
		wantError      string
	}{
		{name: "true omits data", summaryOnly: stringPointer("true"), includeSummary: true, wantStatus: http.StatusOK, wantData: 0},
		{name: "false keeps data", summaryOnly: stringPointer("false"), includeSummary: true, wantStatus: http.StatusOK, wantData: 3},
		{name: "empty means true", summaryOnly: stringPointer(""), includeSummary: true, wantStatus: http.StatusOK, wantData: 0},
		{name: "invalid value is rejected", summaryOnly: stringPointer("definitely-not-bool"), includeSummary: true, wantError: "unexpected value for summaryonly parameter"},
		{name: "summaryonly requires summary", summaryOnly: stringPointer("true"), includeSummary: false, wantError: "summaryonly parameter but no summary fields"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actions.RequireSteveFeature(t, actions.SteveSummaryOptions)
			values := url.Values{
				query.FilterParam: {query.Equal("metadata.labels."+actions.CaseLabel, caseID)},
			}
			if tc.includeSummary {
				values.Set(query.SummaryParam, summaryField)
				values.Set(query.SummaryNamespacedParam, "true")
			}
			if tc.summaryOnly != nil {
				values[query.SummaryOnlyParam] = []string{*tc.summaryOnly}
			}

			status, body := requestConfigMapSummary(t, cl, values)
			if tc.wantError != "" {
				if status < http.StatusBadRequest {
					t.Fatalf("status = %d, want an error containing %q; body=%s", status, tc.wantError, compactBody(body))
				}
				if !strings.Contains(string(body), tc.wantError) {
					t.Fatalf("error body = %s, want substring %q", compactBody(body), tc.wantError)
				}
				return
			}

			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", status, tc.wantStatus, compactBody(body))
			}
			var got summaryResponse
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(got.Data) != tc.wantData {
				t.Fatalf("data length = %d, want %d", len(got.Data), tc.wantData)
			}
			if !reflect.DeepEqual(got.Summary, wantSummary) {
				t.Fatalf("summary = %#v, want exactly %#v", got.Summary, wantSummary)
			}
		})
	}
}

// Decode the expected wire format for the selected version. Modern targets
// remain strict: a numeric response there is a regression, not a legacy fallback.
func decodeSummaryResponse(body []byte, structured bool) (summaryResponse, error) {
	if structured {
		var response summaryResponse
		err := json.Unmarshal(body, &response)
		return response, err
	}
	var legacy struct {
		Data    []map[string]any `json:"data"`
		Summary []struct {
			Property string         `json:"property"`
			Counts   map[string]int `json:"counts"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil {
		return summaryResponse{}, err
	}
	response := summaryResponse{Data: legacy.Data}
	for _, item := range legacy.Summary {
		counts := make(map[string]summaryCount, len(item.Counts))
		for value, total := range item.Counts {
			counts[value] = summaryCount{Total: total}
		}
		response.Summary = append(response.Summary, summaryItem{Property: item.Property, Counts: counts})
	}
	return response, nil
}

func requestConfigMapSummary(t *testing.T, cl *client.Client, values url.Values) (int, []byte) {
	t.Helper()
	schema, ok := cl.Schema("configmap")
	if !ok {
		t.Fatal("ConfigMap schema was not discovered")
	}
	path := schema.CollectionPath
	if len(values) > 0 {
		path += "?" + values.Encode()
	}

	deadline := time.Now().Add(cl.WaitTimeout)
	for {
		resp, body, err := cl.Get(path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusInternalServerError ||
			(!strings.Contains(string(body), "SQL cache sync") && !strings.Contains(string(body), "cache context canceled")) ||
			time.Now().After(deadline) {
			return resp.StatusCode, body
		}
		time.Sleep(cl.PollInterval)
	}
}

func stringPointer(value string) *string {
	return &value
}

func compactBody(body []byte) string {
	const limit = 1000
	compact := strings.TrimSpace(string(body))
	if len(compact) > limit {
		return fmt.Sprintf("%s... (%d bytes)", compact[:limit], len(body))
	}
	return compact
}
