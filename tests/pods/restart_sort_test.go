package pods

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
)

type restartSeed struct {
	name       string
	body       actions.ResourceBodyFactory
	minimumAge time.Duration
	restarted  bool
}

func TestPodsRestartSortScenarios(t *testing.T) {
	actions.RequireSteveFeature(t, actions.StevePodRestartIndexes)
	cl := actions.RequireSQLMode(t).Client
	actions.RequireWorkloads(t)
	tests := []struct {
		name       string
		seeds      []restartSeed
		query      string
		want       []string
		legacyWant []string
	}{
		{name: "pod restart count sort ascending remains numeric",
			seeds: []restartSeed{
				{name: "restart-count-never", body: newNeverRestartPodBody},
				{name: "restart-count-once", restarted: true, body: newRestartOncePodBody},
			}, query: "sort=metadata.fields[3][0]&pagesize=10", want: []string{"restart-count-never", "restart-count-once"}},
		{name: "pod last restart sort ascending",
			seeds: []restartSeed{
				{name: "last-restart-old", restarted: true, body: newRestartOncePodBody, minimumAge: 3 * time.Second},
				{name: "last-restart-young", restarted: true, body: newRestartOncePodBody},
				{name: "last-restart-never", body: newNeverRestartPodBody},
			}, query: "sort=metadata.fields[3][1]&pagesize=10",
			want:       []string{"last-restart-young", "last-restart-old", "last-restart-never"},
			legacyWant: []string{"last-restart-never", "last-restart-old", "last-restart-young"}},
		{name: "pod last restart sort descending",
			seeds: []restartSeed{
				{name: "last-restart-desc-old", restarted: true, body: newRestartOncePodBody, minimumAge: 3 * time.Second},
				{name: "last-restart-desc-young", restarted: true, body: newRestartOncePodBody},
				{name: "last-restart-desc-never", body: newNeverRestartPodBody},
			}, query: "sort=-metadata.fields[3][1]&pagesize=10",
			want:       []string{"last-restart-desc-never", "last-restart-desc-old", "last-restart-desc-young"},
			legacyWant: []string{"last-restart-desc-young", "last-restart-desc-old", "last-restart-desc-never"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ns := actions.CreateTestNamespace(t, cl)
			resources := cl.Resource("pod").Namespace(ns)
			for _, seed := range test.seeds {
				actions.CreateAndWait(t, resources, seed.name, seed.body(ns, seed.name, map[string]string{actions.CaseLabel: "restart-sort"}))
				if seed.minimumAge > 0 {
					waitForPodMinimumRestartAge(t, resources, seed.name, seed.minimumAge)
				}
			}
			// Wait for restart fields before sorting; a newly visible Pod may still
			// have its initial zero restart count in the collection cache.
			for _, seed := range test.seeds {
				if seed.restarted {
					waitForPodLastRestartDuration(t, resources, seed.name)
				}
			}
			want := test.want
			if test.legacyWant != nil && !actions.SteveSupports(actions.SteveRestartAgeSort) {
				// Earlier Steve releases order raw timestamps, before the
				// conversion to elapsed age used by newer releases.
				want = test.legacyWant
				t.Log("Checking legacy restart timestamp ordering")
			}
			coll := actions.WaitForCollection(t, resources, "filter=metadata.labels."+actions.CaseLabel+"=restart-sort&"+test.query, actions.CollectionExpectation{Order: want})
			logPodRestartOrder(t, coll)
		})
	}
}

func waitForPodMinimumRestartAge(t *testing.T, resources *client.ResourceClient, name string, minimumAge time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), podWaitTimeout(resources)+minimumAge)
	defer cancel()

	var lastState string
	for {
		pod, err := resources.GetContext(ctx, name)
		if err == nil {
			finishedAt, found, parseErr := podLastTerminationTime(pod)
			switch {
			case parseErr != nil:
				t.Fatalf("read Pod %q last termination timestamp: %v", name, parseErr)
			case found:
				age := time.Since(finishedAt)
				lastState = fmt.Sprintf("finishedAt=%s age=%s", finishedAt.Format(time.RFC3339Nano), age.Round(time.Millisecond))
				if age >= minimumAge {
					t.Logf("Pod %q restart is at least %s old (%s)", name, minimumAge, lastState)
					return
				}
			default:
				lastState = "lastState.terminated.finishedAt is not present"
			}
		} else if !isRetryablePodRestartError(err) {
			t.Fatalf("GET Pod %q while waiting for minimum restart age: %v", name, err)
		} else {
			lastState = err.Error()
		}
		if !waitForPodPoll(ctx, podPollInterval(resources)) {
			t.Fatalf("timed out waiting for Pod %q restart to become at least %s old; last state: %s", name, minimumAge, lastState)
		}
	}
}

func podLastTerminationTime(item map[string]any) (time.Time, bool, error) {
	statuses := nestedSlice(item, "status", "containerStatuses")
	var latest time.Time
	found := false
	for index, rawStatus := range statuses {
		status, ok := rawStatus.(map[string]any)
		if !ok {
			return time.Time{}, false, fmt.Errorf("containerStatuses[%d] has type %T, want object", index, rawStatus)
		}
		lastState, _ := status["lastState"].(map[string]any)
		terminated, _ := lastState["terminated"].(map[string]any)
		rawFinishedAt, _ := terminated["finishedAt"].(string)
		if strings.TrimSpace(rawFinishedAt) == "" {
			continue
		}
		finishedAt, err := time.Parse(time.RFC3339Nano, rawFinishedAt)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("containerStatuses[%d].lastState.terminated.finishedAt = %q: %w", index, rawFinishedAt, err)
		}
		if !found || finishedAt.After(latest) {
			latest = finishedAt
			found = true
		}
	}
	return latest, found, nil
}

func isRetryablePodRestartError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *client.StatusError
	if !errors.As(err, &statusErr) {
		return true
	}
	return statusErr.StatusCode == http.StatusRequestTimeout ||
		statusErr.StatusCode == http.StatusTooEarly ||
		statusErr.StatusCode == http.StatusTooManyRequests ||
		statusErr.StatusCode >= http.StatusInternalServerError
}

func waitForPodPoll(ctx context.Context, interval time.Duration) bool {
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

func waitForPodLastRestartDuration(t *testing.T, resources *client.ResourceClient, name string) {
	t.Helper()

	rawQuery := "filter=metadata.labels." + actions.CaseLabel + "=restart-sort"
	ctx, cancel := context.WithTimeout(t.Context(), podWaitTimeout(resources))
	defer cancel()

	var lastMatching map[string]any
	var lastErr error
	for {
		coll, err := resources.ListContext(ctx, rawQuery)
		if err == nil && coll != nil {
			lastErr = nil
			for _, item := range coll.Data {
				if nestedString(item, "metadata", "name") != name {
					continue
				}
				lastMatching = item
				if duration := podLastRestartDuration(item); duration != "" {
					t.Logf("Pod %q has restart duration field %q", name, duration)
					return
				}
			}
		} else if err == nil {
			lastErr = fmt.Errorf("Pod collection is nil")
		} else if !isRetryablePodRestartError(err) {
			t.Fatalf("list Pod %q while waiting for restart duration: %v", name, err)
		} else {
			lastErr = err
		}

		if !waitForPodPoll(ctx, podPollInterval(resources)) {
			t.Fatalf("timed out waiting for Pod %q to expose metadata.fields[3][1] last restart duration; last matching item: %s; last error: %v", name, describePodRestartField(lastMatching), lastErr)
		}
	}
}

func podWaitTimeout(resources *client.ResourceClient) time.Duration {
	timeout := resources.WaitTimeout()
	if timeout <= 0 {
		return 30 * time.Second
	}
	return timeout
}

func podPollInterval(resources *client.ResourceClient) time.Duration {
	interval := resources.PollInterval()
	if interval <= 0 {
		return 200 * time.Millisecond
	}
	return interval
}

func podLastRestartDuration(item map[string]any) string {
	fields := nestedSlice(item, "metadata", "fields")
	if len(fields) <= 3 {
		return ""
	}

	switch restarts := fields[3].(type) {
	case []any:
		if len(restarts) <= 1 {
			return ""
		}
		duration, _ := restarts[1].(string)
		return strings.TrimSpace(duration)
	case string:
		return restartDurationFromPrinterField(restarts)
	default:
		return ""
	}
}

func restartDurationFromPrinterField(value string) string {
	start := strings.Index(value, "(")
	end := strings.Index(value, ")")
	if start == -1 || end == -1 || end <= start+1 {
		return ""
	}

	duration := strings.TrimSpace(value[start+1 : end])
	duration = strings.TrimSuffix(duration, " ago")
	return strings.TrimSpace(duration)
}

func describePodRestartField(item map[string]any) string {
	if item == nil {
		return "<not found>"
	}

	fields := nestedSlice(item, "metadata", "fields")
	if len(fields) <= 3 {
		return fmt.Sprintf("metadata.fields=%v phase=%q reason=%q", fields, nestedString(item, "status", "phase"), nestedString(item, "status", "reason"))
	}
	return fmt.Sprintf("metadata.fields[3]=%v phase=%q reason=%q", fields[3], nestedString(item, "status", "phase"), nestedString(item, "status", "reason"))
}

func logPodRestartOrder(t *testing.T, coll *client.SteveCollection) {
	t.Helper()

	for index, item := range coll.Data {
		name := nestedString(item, "metadata", "name")
		t.Logf("Returned pod[%d]: name=%q %s", index, name, describePodRestartField(item))
	}
}

func nestedString(m map[string]any, keys ...string) string {
	var current any = m
	for _, key := range keys {
		currMap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = currMap[key]
	}
	s, _ := current.(string)
	return s
}

func nestedSlice(m map[string]any, keys ...string) []any {
	var current any = m
	for _, key := range keys {
		currMap, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = currMap[key]
	}
	s, _ := current.([]any)
	return s
}
