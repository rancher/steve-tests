package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rancher/steve-tests/pkg/client"
)

func TestUpdateWithConflictRetryRefreshesResourceAfterConflict(t *testing.T) {
	t.Parallel()

	var methods []string
	gets, mutations := 0, 0
	resources := newUpdateTestResource(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.Method)
		if req.URL.Path != "/v1/configmaps/qa/example" {
			t.Errorf("request path = %q, want /v1/configmaps/qa/example", req.URL.Path)
		}
		switch req.Method {
		case http.MethodGet:
			gets++
			return response(http.StatusOK, fmt.Sprintf(`{"metadata":{"name":"example","resourceVersion":"%d"},"data":{"controller":"revision-%d"}}`, gets, gets)), nil
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode update body: %v", err)
			}
			if got := getNestedString(body, "metadata", "resourceVersion"); got != fmt.Sprint(gets) {
				t.Errorf("PUT resourceVersion = %q, want %d", got, gets)
			}
			if got := getNestedString(body, "data", "controller"); got != fmt.Sprintf("revision-%d", gets) {
				t.Errorf("PUT did not preserve fresh controller data: %v", body)
			}
			if got := getNestedString(body, "data", "desired"); got != "changed" {
				t.Errorf("PUT lost mutation: %v", body)
			}
			if gets < 3 {
				return response(http.StatusConflict, `{"message":"resourceVersion is stale"}`), nil
			}
			return response(http.StatusOK, `{"metadata":{"name":"example","resourceVersion":"4"},"server":"accepted"}`), nil
		default:
			t.Fatalf("unexpected method %s", req.Method)
			return nil, nil
		}
	}))

	updated, err := UpdateWithConflictRetry(t.Context(), resources, "example", func(current map[string]any) error {
		mutations++
		data := current["data"].(map[string]any)
		if _, exists := data["desired"]; exists {
			t.Error("retry reused the previous mutated resource")
		}
		data["desired"] = "changed"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateWithConflictRetry: %v", err)
	}
	if mutations != 3 {
		t.Errorf("mutation calls = %d, want 3", mutations)
	}
	if want := []string{"GET", "PUT", "GET", "PUT", "GET", "PUT"}; !equalStrings(methods, want) {
		t.Errorf("request sequence = %v, want %v", methods, want)
	}
	if updated["server"] != "accepted" || getNestedString(updated, "metadata", "resourceVersion") != "4" {
		t.Errorf("returned resource = %v, want the server's PUT response", updated)
	}
}

func TestUpdateWithConflictRetryRejectsUnsafeMutation(t *testing.T) {
	t.Parallel()

	mutationErr := errors.New("mutation cannot be prepared")
	for _, test := range []struct {
		name          string
		body          string
		mutate        func(map[string]any) error
		wantErr       string
		wantCause     error
		wantMutations int
	}{
		{name: "missing metadata", body: `{}`, wantErr: "resource has no metadata object"},
		{name: "invalid metadata", body: `{"metadata":[]}`, wantErr: "resource has no metadata object"},
		{name: "missing version", body: `{"metadata":{}}`, wantErr: "resource has no resourceVersion"},
		{name: "empty version", body: `{"metadata":{"resourceVersion":""}}`, wantErr: "resource has no resourceVersion"},
		{name: "numeric version", body: `{"metadata":{"resourceVersion":1}}`, wantErr: "resource has no resourceVersion"},
		{
			name: "mutation failure", body: `{"metadata":{"resourceVersion":"1"}}`,
			mutate:  func(map[string]any) error { return mutationErr },
			wantErr: "prepare update", wantCause: mutationErr, wantMutations: 1,
		},
		{
			name: "version changed", body: `{"metadata":{"resourceVersion":"1"}}`,
			mutate: func(current map[string]any) error {
				current["metadata"].(map[string]any)["resourceVersion"] = "2"
				return nil
			},
			wantErr: "mutation changed resourceVersion", wantMutations: 1,
		},
		{
			name: "metadata removed", body: `{"metadata":{"resourceVersion":"1"}}`,
			mutate:  func(current map[string]any) error { delete(current, "metadata"); return nil },
			wantErr: "mutation changed resourceVersion", wantMutations: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var methods []string
			resources := newUpdateTestResource(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				methods = append(methods, req.Method)
				return response(http.StatusOK, test.body), nil
			}))
			mutations := 0
			updated, err := UpdateWithConflictRetry(t.Context(), resources, "example", func(current map[string]any) error {
				mutations++
				if test.mutate != nil {
					return test.mutate(current)
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Errorf("error = %v, want wrapped mutation error", err)
			}
			if updated != nil || mutations != test.wantMutations || !equalStrings(methods, []string{"GET"}) {
				t.Errorf("updated=%v, mutation calls=%d, requests=%v; want nil, %d, [GET]", updated, mutations, methods, test.wantMutations)
			}
		})
	}
}

func TestUpdateWithConflictRetryDoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("connection lost")
	for _, test := range []struct {
		name, method string
		status       int
		transportErr error
	}{
		{name: "GET conflict", method: http.MethodGet, status: http.StatusConflict},
		{name: "GET forbidden", method: http.MethodGet, status: http.StatusForbidden},
		{name: "GET transport failure", method: http.MethodGet, transportErr: transportErr},
		{name: "PUT forbidden", method: http.MethodPut, status: http.StatusForbidden},
		{name: "PUT not found", method: http.MethodPut, status: http.StatusNotFound},
		{name: "PUT server error", method: http.MethodPut, status: http.StatusInternalServerError},
		{name: "PUT rate limited", method: http.MethodPut, status: http.StatusTooManyRequests},
		{name: "PUT transport failure", method: http.MethodPut, transportErr: transportErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var methods []string
			resources := newUpdateTestResource(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				methods = append(methods, req.Method)
				if req.Method == test.method {
					if test.transportErr != nil {
						return nil, test.transportErr
					}
					return response(test.status, `{"message":"request failed"}`), nil
				}
				return response(http.StatusOK, `{"metadata":{"resourceVersion":"1"}}`), nil
			}))
			mutations := 0
			updated, err := UpdateWithConflictRetry(t.Context(), resources, "example", func(map[string]any) error {
				mutations++
				return nil
			})
			if err == nil || updated != nil {
				t.Fatalf("updated=%v, error=%v; want nil resource and error", updated, err)
			}
			if test.transportErr != nil {
				if !errors.Is(err, test.transportErr) {
					t.Errorf("error = %v, want wrapped transport error", err)
				}
			} else if !client.IsStatus(err, test.status) {
				t.Errorf("error = %v, want wrapped status %d", err, test.status)
			}
			wantMethods, wantMutations := []string{"GET"}, 0
			if test.method == http.MethodPut {
				wantMethods, wantMutations = []string{"GET", "PUT"}, 1
			}
			if !equalStrings(methods, wantMethods) || mutations != wantMutations {
				t.Errorf("requests=%v, mutation calls=%d; want %v, %d", methods, mutations, wantMethods, wantMutations)
			}
		})
	}
}

func TestUpdateWithConflictRetryStopsBeforeRequest(t *testing.T) {
	t.Parallel()

	for _, nilMutation := range []bool{false, true} {
		t.Run(fmt.Sprintf("nil mutation=%t", nilMutation), func(t *testing.T) {
			t.Parallel()
			requests, mutations := 0, 0
			resources := newUpdateTestResource(roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return response(http.StatusOK, `{}`), nil
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			mutate := func(map[string]any) error { mutations++; return nil }
			if nilMutation {
				mutate = nil
			} else {
				cancel()
			}
			updated, err := UpdateWithConflictRetry(ctx, resources, "example", mutate)
			if nilMutation {
				if err == nil || !strings.Contains(err.Error(), "mutation function is nil") {
					t.Fatalf("error = %v, want nil mutation error", err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context cancellation", err)
			}
			if updated != nil || requests != 0 || mutations != 0 {
				t.Errorf("updated=%v, requests=%d, mutation calls=%d; want nil and no work", updated, requests, mutations)
			}
		})
	}
}

func TestUpdateWithConflictRetryStopsWaitingAfterConflict(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		cancelOnPut  bool
		waitTimeout  time.Duration
		pollInterval time.Duration
		wantCause    error
	}{
		{name: "caller cancellation", cancelOnPut: true, waitTimeout: time.Second, pollInterval: time.Hour, wantCause: context.Canceled},
		{name: "configured deadline", waitTimeout: 200 * time.Millisecond, pollInterval: time.Hour, wantCause: context.DeadlineExceeded},
		{name: "default poll interval", cancelOnPut: true, waitTimeout: time.Second, wantCause: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var methods []string
			cl := newUpdateTestClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				methods = append(methods, req.Method)
				if req.Method == http.MethodPut {
					if test.cancelOnPut {
						cancel()
					}
					return response(http.StatusConflict, `{"message":"controller changed the resource"}`), nil
				}
				return response(http.StatusOK, `{"metadata":{"resourceVersion":"1"}}`), nil
			}))
			cl.WaitTimeout = test.waitTimeout
			cl.PollInterval = test.pollInterval
			updated, err := UpdateWithConflictRetry(ctx, cl.Resource("configmap").Namespace("qa"), "example", func(map[string]any) error { return nil })
			if !errors.Is(err, test.wantCause) {
				t.Fatalf("error = %v, want %v", err, test.wantCause)
			}
			if !strings.Contains(err.Error(), "last conflict:") || !strings.Contains(err.Error(), "controller changed the resource") {
				t.Errorf("error lost last conflict details: %v", err)
			}
			if updated != nil || !equalStrings(methods, []string{"GET", "PUT"}) {
				t.Errorf("updated=%v, requests=%v; want nil and [GET PUT]", updated, methods)
			}
		})
	}
}

func newUpdateTestResource(transport http.RoundTripper) *client.ResourceClient {
	return newUpdateTestClient(transport).Resource("configmap").Namespace("qa")
}

func newUpdateTestClient(transport http.RoundTripper) *client.Client {
	return &client.Client{
		BaseURL:      "https://steve.example.test",
		HTTPClient:   &http.Client{Transport: transport},
		PollInterval: time.Millisecond,
		WaitTimeout:  time.Second,
		Schemas: map[string]client.SchemaInfo{
			"configmap": {ID: "configmap", Namespaced: true, CollectionPath: "v1/configmaps"},
		},
	}
}
