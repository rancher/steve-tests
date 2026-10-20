// Package jobs covers workload-backed synthetic-state indexing regressions.
package jobs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

const jobSchemaID = "batch.job"

// A Job state transition must update collection filters as well as direct GETs.
func TestJobSyntheticStateIndexRefreshes(t *testing.T) {
	tests := []struct {
		name, initialState, finalState string
		deadline                       int64
	}{
		{name: "active to error", initialState: "active", finalState: "error", deadline: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sqlSetup := actions.RequireSQLMode(t)
			workloadSetup := actions.RequireWorkloads(t)
			if sqlSetup.Client != workloadSetup.Client {
				t.Fatal("integration capability gates returned different Steve clients")
			}
			if _, ok := sqlSetup.Client.Schema(jobSchemaID); !ok {
				t.Skipf("target does not expose schema %q", jobSchemaID)
			}

			namespace := actions.CreateTestNamespace(t, sqlSetup.Client)
			resources := sqlSetup.Client.Resource(jobSchemaID).Namespace(namespace)
			name := actions.UniqueName(t, "job-state")
			caseID := actions.UniqueName(t, "job-state-case")
			actions.CreateAndWait(t, resources, name, newFailingJobBody(namespace, name, caseID))

			caseFilter := query.Equal("metadata.labels."+actions.CaseLabel, caseID)
			activeValues := url.Values{query.FilterParam: {
				caseFilter,
				query.Equal("metadata.state.name", test.initialState),
			}}
			if err := resources.WaitForListContainsContext(t.Context(), name, activeValues.Encode(), resources.WaitTimeout()); err != nil {
				t.Fatalf("Job never appeared under metadata.state.name=active: %v", err)
			}

			// Trigger failure only after the active SQL index is observed. Decreasing
			// activeDeadlineSeconds is a mutable Job update and avoids racing a fixed
			// container sleep against API/cache latency on a slow cluster.
			if _, err := actions.UpdateWithConflictRetry(t.Context(), resources, name, func(current map[string]any) error {
				spec, ok := current["spec"].(map[string]any)
				if !ok {
					return fmt.Errorf("Job has no spec object")
				}
				spec["activeDeadlineSeconds"] = test.deadline
				return nil
			}); err != nil {
				t.Fatalf("shorten Job active deadline: %v", err)
			}
			transitionTimeout := resources.WaitTimeout() + 15*time.Second
			waitForJobState(t, resources, name, test.finalState, transitionTimeout)

			errorValues := url.Values{query.FilterParam: {
				caseFilter,
				query.Equal("metadata.state.name", test.finalState),
			}}
			waitForExclusiveStateIndex(t, resources, name, activeValues, errorValues, resources.WaitTimeout())
		})
	}
}

func newFailingJobBody(namespace string, name string, caseID string) map[string]any {
	labels := actions.ManagedLabels(map[string]string{actions.CaseLabel: caseID})
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"activeDeadlineSeconds": int64(3600),
			"backoffLimit":          int64(0),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy":                 "Never",
					"terminationGracePeriodSeconds": int64(0),
					"containers": []any{
						map[string]any{
							"name":            "main",
							"image":           actions.WorkloadImage(),
							"imagePullPolicy": "IfNotPresent",
							"command":         []string{"sh", "-c", "sleep 3600"},
						},
					},
				},
			},
		},
	}
}

func waitForJobState(t *testing.T, resources *client.ResourceClient, name string, expected string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 75*time.Second))
	defer cancel()

	lastState := ""
	for {
		resource, err := resources.GetContext(ctx, name)
		if err == nil {
			lastState = nestedJobString(resource, "metadata", "state", "name")
			if lastState == expected {
				return
			}
		} else if !client.IsStatus(err,
			http.StatusRequestTimeout,
			http.StatusTooEarly,
			http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		) {
			t.Fatalf("GET Job while waiting for state %q: %v", expected, err)
		}
		if !waitForNextPoll(ctx, resources.PollInterval()) {
			t.Fatalf("timed out waiting for Job %q state %q; last state=%q", name, expected, lastState)
		}
	}
}

func waitForExclusiveStateIndex(
	t *testing.T,
	resources *client.ResourceClient,
	name string,
	oldValues url.Values,
	newValues url.Values,
	timeout time.Duration,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), positiveDuration(timeout, 30*time.Second))
	defer cancel()

	var oldNames, newNames []string
	var lastErr error
	for {
		oldCollection, oldErr := resources.ListValuesContext(ctx, oldValues)
		newCollection, newErr := resources.ListValuesContext(ctx, newValues)
		if oldErr == nil && newErr == nil {
			oldNames, oldErr = jobCollectionNames(oldCollection)
			newNames, newErr = jobCollectionNames(newCollection)
			if oldErr == nil && newErr == nil && !containsJobName(oldNames, name) && containsJobName(newNames, name) {
				return
			}
		}
		lastErr = firstJobError(oldErr, newErr)
		if lastErr != nil && !isRetryableJobError(lastErr) {
			t.Fatalf("query Job state indexes: %v", lastErr)
		}
		if !waitForNextPoll(ctx, resources.PollInterval()) {
			t.Fatalf("timed out waiting for Job %q to leave active index and enter error index; active=%v error=%v lastError=%v", name, oldNames, newNames, lastErr)
		}
	}
}

func jobCollectionNames(collection *client.SteveCollection) ([]string, error) {
	if collection == nil {
		return nil, fmt.Errorf("Job collection is nil")
	}
	names := make([]string, 0, len(collection.Data))
	for index, item := range collection.Data {
		name := nestedJobString(item, "metadata", "name")
		if name == "" {
			return nil, fmt.Errorf("Job collection item %d has no metadata.name", index)
		}
		names = append(names, name)
	}
	return names, nil
}

func nestedJobString(value map[string]any, keys ...string) string {
	var current any = value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	result, _ := current.(string)
	return result
}

func containsJobName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func firstJobError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func isRetryableJobError(err error) bool {
	return client.IsStatus(err,
		http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	)
}

func waitForNextPoll(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func positiveDuration(value time.Duration, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}
