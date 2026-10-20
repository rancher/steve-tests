// Package contracts contains read-only wire-level checks for Steve and its
// Kubernetes proxy. These run without the mutation safety gate.
package contracts

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
)

const contractRequestTimeout = 10 * time.Second

func TestReadOnlyAPISurfacesReturnJSON(t *testing.T) {
	client := actions.IntegrationClient(t)
	tests := []struct {
		name     string
		path     string
		validate func([]byte) error
	}{
		{name: "Kubernetes version", path: "version", validate: validateVersionResponse},
		{name: "core API versions", path: "api", validate: func(body []byte) error {
			return validateStringArrayField(body, "versions", "v1")
		}},
		{name: "grouped API versions", path: "apis", validate: func(body []byte) error {
			return validateObjectArrayField(body, "groups", "name")
		}},
		{name: "Steve schemas", path: "v1/schemas?pagesize=1", validate: func(body []byte) error {
			return validateObjectArrayField(body, "data", "id")
		}},
		{name: "Steve counts", path: "v1/counts?exclude=metadata.managedFields", validate: func(body []byte) error {
			return validateObjectArrayField(body, "data", "counts")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), contractRequestTimeout)
			defer cancel()
			response, body, err := client.GetContext(ctx, test.path)
			if err != nil {
				t.Fatalf("GET %s: %v", test.path, err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d: %s", test.path, response.StatusCode, responseBodySummary(body))
			}
			if err := validateJSONContentType(response.Header.Get("Content-Type")); err != nil {
				t.Fatalf("GET %s: %v", test.path, err)
			}
			if err := test.validate(body); err != nil {
				t.Fatalf("GET %s violated its JSON contract: %v; body: %s", test.path, err, responseBodySummary(body))
			}
		})
	}
}

// Core collection requests must finish within the deadline.
func TestSteveCollectionSurfacesRemainResponsive(t *testing.T) {
	client := actions.IntegrationClient(t)
	probes := []struct {
		name       string
		schemaID   string
		issueQuery string
	}{
		{
			name:       "namespaces",
			schemaID:   "namespace",
			issueQuery: "exclude=metadata.managedFields&pagesize=1",
		},
	}

	for _, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			schema, ok := client.Schema(probe.schemaID)
			if !ok {
				t.Fatalf("target does not expose required Steve schema %q", probe.schemaID)
			}
			if !contains(schema.CollectionMethods, http.MethodGet) {
				t.Fatalf("required Steve schema %q does not advertise collection GET", schema.ID)
			}

			path := schema.CollectionPath + "?" + probe.issueQuery
			ctx, cancel := context.WithTimeout(t.Context(), contractRequestTimeout)
			defer cancel()
			response, body, err := client.GetContext(ctx, path)
			if err != nil {
				t.Fatalf("GET %s did not complete within the collection responsiveness deadline: %v", path, err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d: %s", path, response.StatusCode, responseBodySummary(body))
			}
			if err := validateJSONContentType(response.Header.Get("Content-Type")); err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			if err := validateCollectionEnvelope(body); err != nil {
				t.Fatalf("GET %s violated the Steve collection contract: %v; body: %s", path, err, responseBodySummary(body))
			}
		})
	}
}

// Collections backed by APIs without a list verb must return without waiting
// for a list/watch informer that cannot synchronize.
func TestCollectionsWithoutGETMethodDoNotHang(t *testing.T) {
	setup := actions.SetupIntegration(t)
	if setup.CacheMode == "sql" {
		// Older SQL stores start an informer that can never synchronize for
		// these APIs. A pending schema refresh can then block other resources.
		actions.RequireSteveFeature(t, actions.SteveNonListableCollectionGuard)
	}
	client := setup.Client
	tested := 0
	for _, schema := range client.Schemas {
		if schema.Links["collection"] == "" || contains(schema.CollectionMethods, http.MethodGet) {
			continue
		}
		schema := schema
		t.Run(schema.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), contractRequestTimeout)
			defer cancel()
			response, body, err := client.GetContext(ctx, schema.CollectionPath)
			if err != nil {
				t.Fatalf("GET collection for %s did not complete: %v", schema.ID, err)
			}
			if response.StatusCode >= http.StatusInternalServerError {
				t.Fatalf("GET collection for %s returned status %d: %s", schema.ID, response.StatusCode, responseBodySummary(body))
			}
			if len(body) > 0 {
				if err := validateJSONContentType(response.Header.Get("Content-Type")); err != nil {
					t.Fatalf("GET collection for %s: %v", schema.ID, err)
				}
				if !json.Valid(body) {
					t.Fatalf("GET collection for %s returned invalid JSON: %s", schema.ID, responseBodySummary(body))
				}
			}
		})
		tested++
	}
	if tested == 0 {
		t.Skip("target does not expose a collection without GET in collectionMethods")
	}
}

func TestReadOnlyAPIContractValidators(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		validate func([]byte) error
		wantErr  string
	}{
		{
			name:     "version",
			body:     `{"gitVersion":"v1.34.0"}`,
			validate: validateVersionResponse,
		},
		{
			name:     "version missing gitVersion",
			body:     `{}`,
			validate: validateVersionResponse,
			wantErr:  "gitVersion must be a non-empty string",
		},
		{
			name: "core versions",
			body: `{"versions":["v1"]}`,
			validate: func(body []byte) error {
				return validateStringArrayField(body, "versions", "v1")
			},
		},
		{
			name: "core versions missing v1",
			body: `{"versions":["v2"]}`,
			validate: func(body []byte) error {
				return validateStringArrayField(body, "versions", "v1")
			},
			wantErr: `does not contain required value "v1"`,
		},
		{
			name: "object array",
			body: `{"data":[{"id":"configmap"}]}`,
			validate: func(body []byte) error {
				return validateObjectArrayField(body, "data", "id")
			},
		},
		{
			name: "empty object array",
			body: `{"data":[]}`,
			validate: func(body []byte) error {
				return validateObjectArrayField(body, "data", "id")
			},
			wantErr: `field "data" must not be empty`,
		},
		{
			name: "object array item missing required field",
			body: `{"data":[{}]}`,
			validate: func(body []byte) error {
				return validateObjectArrayField(body, "data", "id")
			},
			wantErr: `must contain non-null field "id"`,
		},
		{
			name: "object array later item missing required field",
			body: `{"data":[{"id":"configmap"},{}]}`,
			validate: func(body []byte) error {
				return validateObjectArrayField(body, "data", "id")
			},
			wantErr: `item 2 must contain non-null field "id"`,
		},
		{
			name: "object array string identity cannot be empty",
			body: `{"data":[{"id":""}]}`,
			validate: func(body []byte) error {
				return validateObjectArrayField(body, "data", "id")
			},
			wantErr: `field "id" must be a non-empty string`,
		},
		{
			name: "object array string identity rejects wrong type",
			body: `{"data":[{"id":42}]}`,
			validate: func(body []byte) error {
				return validateObjectArrayField(body, "data", "id")
			},
			wantErr: `field "id" must be a non-empty string`,
		},
		{
			name: "counts item requires object counts",
			body: `{"data":[{"counts":"wrong"}]}`,
			validate: func(body []byte) error {
				return validateObjectArrayField(body, "data", "counts")
			},
			wantErr: `field "counts" must be an object`,
		},
		{
			name:     "empty collection envelope is valid",
			body:     `{"data":[]}`,
			validate: validateCollectionEnvelope,
		},
		{
			name:     "collection envelope data cannot be null",
			body:     `{"data":null}`,
			validate: validateCollectionEnvelope,
			wantErr:  `field "data" must be a JSON array`,
		},
		{
			name:     "collection envelope items cannot be null",
			body:     `{"data":[null]}`,
			validate: validateCollectionEnvelope,
			wantErr:  `item 1 must be an object`,
		},
		{
			name: "top level null",
			body: `null`,
			validate: func(body []byte) error {
				_, err := decodeJSONObject(body)
				return err
			},
			wantErr: "response must be a JSON object",
		},
		{
			name: "invalid JSON",
			body: `{`,
			validate: func(body []byte) error {
				_, err := decodeJSONObject(body)
				return err
			},
			wantErr: "decode JSON object",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.validate([]byte(test.body))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validator returned error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
			}
		})
	}
}

func TestValidateJSONContentType(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		value   string
		wantErr bool
	}{
		{value: "application/json"},
		{value: "application/json; charset=utf-8"},
		{value: "text/plain", wantErr: true},
		{value: "text/application/json", wantErr: true},
		{value: "", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			err := validateJSONContentType(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateJSONContentType(%q) error = %v, wantErr=%t", test.value, err, test.wantErr)
			}
		})
	}
}

func validateJSONContentType(value string) error {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return fmt.Errorf("invalid Content-Type %q: %w", value, err)
	}
	if mediaType != "application/json" {
		return fmt.Errorf("Content-Type = %q, want application/json", value)
	}
	return nil
}

func validateVersionResponse(body []byte) error {
	document, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	var version string
	if err := json.Unmarshal(document["gitVersion"], &version); err != nil || strings.TrimSpace(version) == "" {
		return fmt.Errorf("gitVersion must be a non-empty string")
	}
	return nil
}

func validateStringArrayField(body []byte, field string, requiredValue string) error {
	document, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	var values []string
	if err := json.Unmarshal(document[field], &values); err != nil {
		return fmt.Errorf("field %q must be an array of strings: %w", field, err)
	}
	if len(values) == 0 {
		return fmt.Errorf("field %q must not be empty", field)
	}
	if requiredValue != "" && !contains(values, requiredValue) {
		return fmt.Errorf("field %q does not contain required value %q", field, requiredValue)
	}
	return nil
}

func validateObjectArrayField(body []byte, field string, requiredItemField string) error {
	document, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(document[field], &values); err != nil {
		return fmt.Errorf("field %q must be an array of objects: %w", field, err)
	}
	if len(values) == 0 {
		return fmt.Errorf("field %q must not be empty", field)
	}
	for index, item := range values {
		if item == nil {
			return fmt.Errorf("%q item %d must be an object", field, index+1)
		}
		raw, present := item[requiredItemField]
		if !present || len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("%q item %d must contain non-null field %q", field, index+1, requiredItemField)
		}
		switch requiredItemField {
		case "id", "name":
			var value string
			if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
				return fmt.Errorf("%q item %d field %q must be a non-empty string", field, index+1, requiredItemField)
			}
		case "counts":
			var value map[string]json.RawMessage
			if err := json.Unmarshal(raw, &value); err != nil || value == nil {
				return fmt.Errorf("%q item %d field %q must be an object", field, index+1, requiredItemField)
			}
		}
	}
	return nil
}

func validateCollectionEnvelope(body []byte) error {
	document, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	raw, present := document["data"]
	if !present || len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("field %q must be a JSON array", "data")
	}
	var data []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("field %q must be an array of objects: %w", "data", err)
	}
	for index, item := range data {
		if item == nil {
			return fmt.Errorf("field %q item %d must be an object", "data", index+1)
		}
	}
	return nil
}

func decodeJSONObject(body []byte) (map[string]json.RawMessage, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("decode JSON object: %w", err)
	}
	if document == nil {
		return nil, fmt.Errorf("response must be a JSON object")
	}
	return document, nil
}

func responseBodySummary(body []byte) string {
	const max = 512
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) <= max {
		return trimmed
	}
	return trimmed[:max] + "..."
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
