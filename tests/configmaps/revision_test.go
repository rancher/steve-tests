package configmaps

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

// Releases with revision validation reject a minimum resource version newer
// than the cache. Older releases ignore this parameter and return current data.
func TestConfigMapsFutureRevisionIsRejected(t *testing.T) {
	t.Parallel()
	setup := actions.RequireSQLMode(t)
	namespace := actions.CreateTestNamespace(t, setup.Client)
	resources := setup.Client.Resource("configmap").Namespace(namespace)
	name := actions.UniqueName(t, "future-revision")
	caseID := actions.UniqueName(t, "future-revision-case")
	actions.CreateAndWait(t, resources, name, newConfigMapBody(namespace, name, map[string]string{actions.CaseLabel: caseID}))

	base := query.ListOptions{
		Filters:  []string{query.Equal("metadata.labels."+actions.CaseLabel, caseID)},
		Sort:     []string{"metadata.name"},
		PageSize: 10,
	}
	values, err := base.Values()
	if err != nil {
		t.Fatalf("build baseline list query: %v", err)
	}
	baseline := waitForRevisionBaseline(t, resources, values, name)

	var revision int64
	if actions.SteveSupports(actions.SteveCollectionRevision) {
		revision, err = strconv.ParseInt(baseline.Revision, 10, 64)
		if err != nil || revision < 0 {
			t.Fatalf("baseline revision = %q, want a non-negative base-10 int64", baseline.Revision)
		}
	} else if baseline.Revision != "" {
		t.Fatalf("legacy collection revision = %q, want the field omitted", baseline.Revision)
	}

	// Zero is safe across HA replicas. MaxInt64 cannot be overtaken by a busy
	// cluster between reading the baseline and making the next request.
	tests := []struct {
		name, revision, wantError string
		wantStatus                int
	}{
		{name: "known revision", revision: "0"},
		{name: "future revision", revision: strconv.FormatInt(math.MaxInt64, 10), wantStatus: http.StatusBadRequest, wantError: "unknown revision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.wantStatus != 0 && revision == math.MaxInt64 {
				t.Skip("cache revision is already math.MaxInt64")
			}
			options := base
			options.Revision = test.revision
			values, err := options.Values()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			collection, err := resources.ListValuesContext(ctx, values)
			if test.wantStatus == 0 {
				if err != nil {
					t.Fatalf("known revision rejected: %v", err)
				}
				return
			}
			if !actions.SteveSupports(actions.SteveFutureRevisionValidation) {
				if err != nil {
					t.Fatalf("legacy revision parameter rejected: %v", err)
				}
				names := collectionNames(collection)
				if len(collection.Data) != 1 || len(names) != 1 || names[0] != name {
					t.Fatalf("legacy revision query returned names %v, want current fixture %q", names, name)
				}
				t.Log("Checking current data returned by a release that ignores revision")
				return
			}
			if !client.IsStatus(err, test.wantStatus) {
				t.Fatalf("revision %s returned %v, want HTTP %d", test.revision, err, test.wantStatus)
			}
			var statusErr *client.StatusError
			if !errors.As(err, &statusErr) || !strings.Contains(strings.ToLower(statusErr.Body), test.wantError) {
				t.Fatalf("revision %s returned %s, want %q", test.revision, compactRevisionError(err), test.wantError)
			}
		})
	}

}

func waitForRevisionBaseline(t *testing.T, resources *client.ResourceClient, values url.Values, name string) *client.SteveCollection {
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
			lastNames = collectionNames(collection)
			if len(collection.Data) == 1 && len(lastNames) == 1 && lastNames[0] == name {
				return collection
			}
		} else if !isRetryableRevisionListError(err) {
			t.Fatalf("list baseline revision: %v", err)
		}
		lastErr = err

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("timed out waiting for revision baseline %q; last names=%v last error=%v", name, lastNames, lastErr)
		case <-timer.C:
		}
	}
}

func isRetryableRevisionListError(err error) bool {
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

func compactRevisionError(err error) string {
	var statusErr *client.StatusError
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("%s %s status=%d", statusErr.Method, statusErr.Path, statusErr.StatusCode)
	}
	return err.Error()
}
