package deployments

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

const associatedReadinessAnnotation = "steve-tests.cattle.io/ready"

// Keep the same child alive throughout: changing membership cannot hide a stale
// associated-state cache. A projected annotation controls the readiness probe.
func TestReplicaSetAssociatedDataStateTransitions(t *testing.T) {
	cl := associatedDataClient(t)
	ns := actions.CreateTestNamespace(t, cl)
	replicaSets := cl.Resource("apps.replicaset").Namespace(ns)
	pods := cl.Resource("pod").Namespace(ns)
	name := "associated-readiness"
	caseID := actions.UniqueName(t, "associated-readiness")
	body := newReplicaSetBody(ns, name, caseID)
	template := body["spec"].(map[string]any)["template"].(map[string]any)
	template["metadata"].(map[string]any)["annotations"] = map[string]string{
		associatedReadinessAnnotation: "true",
	}
	podSpec := template["spec"].(map[string]any)
	podSpec["volumes"] = []map[string]any{{
		"name": "readiness",
		"downwardAPI": map[string]any{
			"items": []map[string]any{{
				"path": "ready",
				"fieldRef": map[string]any{
					"apiVersion": "v1",
					"fieldPath":  "metadata.annotations['" + associatedReadinessAnnotation + "']",
				},
			}},
		},
	}}
	container := podSpec["containers"].([]map[string]any)[0]
	// Mount the directory, not a subPath: projected files must receive updates.
	container["volumeMounts"] = []map[string]any{{
		"name": "readiness", "mountPath": "/var/run/steve-ready", "readOnly": true,
	}}
	container["readinessProbe"] = map[string]any{
		"exec": map[string]any{
			"command": []string{"sh", "-c", `test "$(cat /var/run/steve-ready/ready)" = true`},
		},
		"periodSeconds":    int64(1),
		"timeoutSeconds":   int64(1),
		"failureThreshold": int64(1),
		"successThreshold": int64(1),
	}
	actions.CreateAndWait(t, replicaSets, name, body)
	initialPods := waitForReplicaSetPods(t, pods, name, 1)
	podName := nestedStringValue(initialPods[0], "metadata", "name")
	podUID := nestedStringValue(initialPods[0], "metadata", "uid")
	if podName == "" || podUID == "" {
		t.Fatal("ready Pod has no name or UID")
	}
	initialState := initialPods[0]["metadata"].(map[string]any)["state"]
	options := associatedListOptions(caseID, query.Bool(true))
	parentID := ns + "/" + name
	waitForAssociatedPodStates(t, replicaSets, options, map[string][]map[string]any{parentID: initialPods})

	for _, step := range []struct {
		name       string
		annotation string
		ready      bool
	}{
		{name: "becomes unready", annotation: "false", ready: false},
		{name: "recovers", annotation: "true", ready: true},
	} {
		if !t.Run(step.name, func(t *testing.T) {
			if _, err := actions.UpdateWithConflictRetry(t.Context(), pods, podName, func(current map[string]any) error {
				metadata, ok := current["metadata"].(map[string]any)
				if !ok || metadata["uid"] != podUID {
					return fmt.Errorf("original Pod %q was replaced", podName)
				}
				annotations, ok := metadata["annotations"].(map[string]any)
				if !ok {
					return fmt.Errorf("Pod %q has no annotations object", podName)
				}
				annotations[associatedReadinessAnnotation] = step.annotation
				return nil
			}); err != nil {
				t.Fatalf("change Pod readiness annotation: %v", err)
			}

			current := waitForAssociatedPodReadiness(t, pods, podName, podUID, step.ready, initialState)
			waitForAssociatedPodStates(t, replicaSets, options, map[string][]map[string]any{
				parentID: {current},
			})
		}) {
			return
		}
	}
}

func waitForAssociatedPodReadiness(t *testing.T, pods *client.ResourceClient, name, uid string, ready bool, initialState any) map[string]any {
	t.Helper()
	var observed map[string]any
	// Projected volumes refresh on kubelet's sync cycle, which can exceed the
	// harness's default 30-second timeout. Only this readiness wait gets a floor;
	// the associated-data cache must still converge within the configured timeout.
	timeout := max(pods.WaitTimeout(), 3*time.Minute)
	waitForAssociatedConditionTimeout(t, pods, timeout, fmt.Sprintf("Pod %s Ready=%t and updated Steve state", name, ready), func(ctx context.Context) error {
		pod, err := pods.GetContext(ctx, name)
		if err != nil {
			return err
		}
		if nestedStringValue(pod, "metadata", "uid") != uid {
			return fmt.Errorf("original Pod %q was replaced", name)
		}
		if !associatedPodHasReadiness(pod, ready) {
			return fmt.Errorf("Pod %q Ready condition is not %t yet", name, ready)
		}
		metadata, _ := pod["metadata"].(map[string]any)
		state, ok := metadata["state"].(map[string]any)
		if !ok || state["name"] == "" || state["name"] == nil {
			return fmt.Errorf("Pod %q has no Steve state", name)
		}
		if ready && !reflect.DeepEqual(state, initialState) {
			return fmt.Errorf("Pod %q has not recovered its original Steve state: %v", name, state)
		}
		if !ready && reflect.DeepEqual(state, initialState) {
			return fmt.Errorf("Pod %q still has its original healthy Steve state: %v", name, state)
		}
		observed = pod
		return nil
	})
	return observed
}
