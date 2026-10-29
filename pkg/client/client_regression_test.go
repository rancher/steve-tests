package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type waitRegressionOperation struct {
	name          string
	call          func(*ResourceClient, context.Context, time.Duration) error
	successStatus int
	successBody   string
}

func waitRegressionOperations() []waitRegressionOperation {
	return []waitRegressionOperation{
		{
			name: "get",
			call: func(resource *ResourceClient, ctx context.Context, timeout time.Duration) error {
				_, err := resource.WaitForGetContext(ctx, "example", timeout)
				return err
			},
			successStatus: http.StatusOK,
			successBody:   `{"metadata":{"name":"example","namespace":"qa"}}`,
		},
		{
			name: "delete",
			call: func(resource *ResourceClient, ctx context.Context, timeout time.Duration) error {
				return resource.WaitForDeleteContext(ctx, "example", timeout)
			},
			successStatus: http.StatusGone,
		},
		{
			name: "list contains",
			call: func(resource *ResourceClient, ctx context.Context, timeout time.Duration) error {
				return resource.WaitForListContainsContext(ctx, "example", "", timeout)
			},
			successStatus: http.StatusOK,
			successBody:   `{"data":[{"metadata":{"name":"example","namespace":"qa"}}]}`,
		},
		{
			name: "list not contains",
			call: func(resource *ResourceClient, ctx context.Context, timeout time.Duration) error {
				return resource.WaitForListNotContainsContext(ctx, "example", "", timeout)
			},
			successStatus: http.StatusOK,
			successBody:   `{"data":[]}`,
		},
	}
}

func regressionResourceClient(transport http.RoundTripper) *ResourceClient {
	client := &Client{
		BaseURL:      "https://steve.example.test",
		HTTPClient:   &http.Client{Transport: transport},
		PollInterval: time.Millisecond,
		Schemas: map[string]SchemaInfo{
			"configmap": {ID: "configmap", Namespaced: true, CollectionPath: "v1/configmaps"},
		},
	}
	return client.Resource("configmap").Namespace("qa")
}

func regressionResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestWaitHelpersRetryTransientHTTPFailures(t *testing.T) {
	t.Parallel()
	for _, operation := range waitRegressionOperations() {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			resource := regressionResourceClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch requests.Add(1) {
				case 1:
					return regressionResponse(request, http.StatusTooManyRequests, "rate limited"), nil
				case 2:
					return regressionResponse(request, http.StatusServiceUnavailable, "temporarily unavailable"), nil
				default:
					return regressionResponse(request, operation.successStatus, operation.successBody), nil
				}
			}))
			if err := operation.call(resource, context.Background(), time.Second); err != nil {
				t.Fatalf("wait returned error after transient failures: %v", err)
			}
			if got := requests.Load(); got != 3 {
				t.Fatalf("requests = %d, want 3", got)
			}
		})
	}
}

func TestWaitHelpersReturnPermanentHTTPFailuresWithoutRetry(t *testing.T) {
	t.Parallel()
	for _, operation := range waitRegressionOperations() {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			resource := regressionResourceClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				return regressionResponse(request, http.StatusForbidden, "permission denied"), nil
			}))
			err := operation.call(resource, context.Background(), time.Second)
			if !IsStatus(err, http.StatusForbidden) {
				t.Fatalf("wait error = %v, want forbidden StatusError", err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

func TestWaitHelpersPreserveCancellationAndLastFailure(t *testing.T) {
	t.Parallel()
	for _, operation := range waitRegressionOperations() {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests atomic.Int32
			resource := regressionResourceClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				cancel()
				return regressionResponse(request, http.StatusServiceUnavailable, "cache still warming"), nil
			}))
			// Cancellation must interrupt the poll delay, even when it is long.
			resource.client.PollInterval = time.Hour
			err := operation.call(resource, ctx, time.Second)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("wait error = %v, want context.Canceled", err)
			}
			if !strings.Contains(err.Error(), "last error:") || !strings.Contains(err.Error(), "cache still warming") {
				t.Fatalf("wait error omitted last failure: %v", err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

func TestWaitHelpersBoundInFlightRequestsByTimeout(t *testing.T) {
	t.Parallel()
	for _, operation := range waitRegressionOperations() {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			resource := regressionResourceClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				if _, ok := request.Context().Deadline(); !ok {
					return nil, errors.New("wait did not attach a request deadline")
				}
				<-request.Context().Done()
				return nil, request.Context().Err()
			}))
			err := operation.call(resource, context.Background(), 100*time.Millisecond)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wait error = %v, want context.DeadlineExceeded", err)
			}
			if !strings.Contains(err.Error(), "timed out waiting for") || !strings.Contains(err.Error(), "configmap/example") {
				t.Fatalf("wait error lacks operation context: %v", err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

func TestResourceOperationsPreserveHTTPErrorDetails(t *testing.T) {
	t.Parallel()
	for _, operation := range []struct {
		name, method, path string
		call               func(*ResourceClient) error
	}{
		{name: "create", method: http.MethodPost, path: "v1/configmaps/qa", call: func(resource *ResourceClient) error {
			_, err := resource.Create(map[string]any{})
			return err
		}},
		{name: "get", method: http.MethodGet, path: "v1/configmaps/qa/example", call: func(resource *ResourceClient) error {
			_, err := resource.Get("example")
			return err
		}},
		{name: "update", method: http.MethodPut, path: "v1/configmaps/qa/example", call: func(resource *ResourceClient) error {
			_, err := resource.Update("example", map[string]any{})
			return err
		}},
		{name: "patch", method: http.MethodPatch, path: "v1/configmaps/qa/example", call: func(resource *ResourceClient) error {
			_, err := resource.Patch("example", map[string]any{}, "application/merge-patch+json")
			return err
		}},
		{name: "delete", method: http.MethodDelete, path: "v1/configmaps/qa/example", call: func(resource *ResourceClient) error {
			return resource.Delete("example")
		}},
		{name: "list", method: http.MethodGet, path: "v1/configmaps/qa", call: func(resource *ResourceClient) error {
			_, err := resource.ListValues(nil)
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			resource := regressionResourceClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return regressionResponse(request, http.StatusConflict, "  resource version conflict\n"), nil
			}))
			err := operation.call(resource)
			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("operation error = %v, want StatusError", err)
			}
			if statusErr.Method != operation.method || statusErr.Path != operation.path || statusErr.StatusCode != http.StatusConflict || statusErr.Body != "resource version conflict" {
				t.Fatalf("status error details = %#v", statusErr)
			}
		})
	}
}

func TestResourceResponsesRejectMalformedJSONWithRequestContext(t *testing.T) {
	t.Parallel()
	for _, collection := range []bool{false, true} {
		name := "resource"
		if collection {
			name = "collection"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resource := regressionResourceClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return regressionResponse(request, http.StatusOK, `{"data":`), nil
			}))
			var err error
			if collection {
				_, err = resource.List("")
			} else {
				_, err = resource.Get("example")
			}
			var syntaxErr *json.SyntaxError
			if !errors.As(err, &syntaxErr) {
				t.Fatalf("operation error = %v, want wrapped JSON syntax error", err)
			}
			if !strings.Contains(err.Error(), "decode Steve GET v1/configmaps/qa") {
				t.Fatalf("decode error lacks request context: %v", err)
			}
		})
	}
}

type failingRegressionBody struct {
	closed bool
	err    error
}

func (body *failingRegressionBody) Read(buffer []byte) (int, error) {
	return copy(buffer, "partial response"), body.err
}

func (body *failingRegressionBody) Close() error {
	body.closed = true
	return nil
}

func TestClientClosesResponseAndRejectsPartialBodyOnReadFailure(t *testing.T) {
	t.Parallel()
	readErr := errors.New("response stream interrupted")
	body := &failingRegressionBody{err: readErr}
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	resource := regressionResourceClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response, nil
	}))
	gotResponse, gotBody, err := resource.client.Get("v1/configmaps")
	if !errors.Is(err, readErr) {
		t.Fatalf("request error = %v, want read failure", err)
	}
	if gotResponse != response || gotBody != nil {
		t.Fatalf("request returned response=%p body=%q, want original response and no partial body", gotResponse, gotBody)
	}
	if !body.closed {
		t.Fatal("response body was not closed after read failure")
	}
}
