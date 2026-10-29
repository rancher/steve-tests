package secrets

import (
	"encoding/base64"
	"testing"

	"github.com/rancher/steve-tests/actions"
)

// stringData is write-only: it overrides matching data keys on writes, while
// reads expose base64 data. Updates must preserve keys that were not replaced.
func TestSecretDataRoundTrip(t *testing.T) {
	t.Parallel()
	cl := actions.RequireMutations(t).Client
	namespace := actions.CreateTestNamespace(t, cl)
	resources := cl.Resource("secret").Namespace(namespace)
	name := actions.UniqueName(t, "secret-data")

	const binaryValue = "\x00\xff\x80\n"
	const textValue = "first line\n日本語\n"
	body := newSecretBody(namespace, name, map[string]string{actions.CaseLabel: name})
	body["data"] = map[string]string{
		"binary":  base64.StdEncoding.EncodeToString([]byte(binaryValue)),
		"replace": base64.StdEncoding.EncodeToString([]byte("encoded-before")),
	}
	body["stringData"] = map[string]string{
		"replace": "string-data-wins",
		"empty":   "",
		"text":    textValue,
	}
	actions.CreateAndWait(t, resources, name, body)

	fetched, err := resources.GetContext(t.Context(), name)
	if err != nil {
		t.Fatalf("get Secret after create: %s", secretMetadataError(err))
	}
	want := map[string]string{
		"binary":  binaryValue,
		"replace": "string-data-wins",
		"empty":   "",
		"text":    textValue,
	}
	assertSecretData(t, "after create", fetched, want)

	updated, err := actions.UpdateWithConflictRetry(t.Context(), resources, name, func(current map[string]any) error {
		current["stringData"] = map[string]string{
			"replace": "updated-string-data",
			"added":   "new-key",
		}
		return nil
	})
	if err != nil {
		t.Fatalf("update Secret data: %s", secretMetadataError(err))
	}
	want["replace"] = "updated-string-data"
	want["added"] = "new-key"
	assertSecretData(t, "update response", updated, want)

	fetched, err = resources.GetContext(t.Context(), name)
	if err != nil {
		t.Fatalf("get Secret after update: %s", secretMetadataError(err))
	}
	assertSecretData(t, "after update", fetched, want)
}

// Report key names and types, never Secret payloads.
func assertSecretData(t *testing.T, phase string, resource map[string]any, expected map[string]string) {
	t.Helper()
	if _, present := resource["stringData"]; present {
		t.Fatalf("%s: write-only stringData field was returned", phase)
	}
	data, ok := resource["data"].(map[string]any)
	if !ok {
		t.Fatalf("%s: Secret data has type %T, want object", phase, resource["data"])
	}
	if len(data) != len(expected) {
		t.Fatalf("%s: Secret data has %d keys, want %d", phase, len(data), len(expected))
	}
	for key, want := range expected {
		encoded, ok := data[key].(string)
		if !ok {
			t.Fatalf("%s: Secret data key %q has type %T, want base64 string", phase, key, data[key])
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("%s: Secret data key %q is not valid base64", phase, key)
		}
		if string(decoded) != want {
			t.Fatalf("%s: Secret data key %q did not preserve its expected value", phase, key)
		}
	}
}
