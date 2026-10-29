package deployments

import (
	"context"
	"fmt"
	"testing"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

func TestReplicaSetAssociatedDataScaling(t *testing.T) {
	cl := associatedDataClient(t)
	ns := actions.CreateTestNamespace(t, cl)
	replicaSets := cl.Resource("apps.replicaset").Namespace(ns)
	pods := cl.Resource("pod").Namespace(ns)
	name := "associated-scaling"
	caseID := actions.UniqueName(t, "associated-scaling")
	actions.CreateAndWait(t, replicaSets, name, newReplicaSetBody(ns, name, caseID))

	for step, count := range []int{1, 3, 1, 0} {
		if !t.Run(fmt.Sprintf("step_%d_replicas_%d", step, count), func(t *testing.T) {
			if step > 0 {
				_, err := actions.UpdateWithConflictRetry(t.Context(), replicaSets, name, func(resource map[string]any) error {
					spec, ok := resource["spec"].(map[string]any)
					if !ok {
						return fmt.Errorf("ReplicaSet spec has type %T, want object", resource["spec"])
					}
					spec["replicas"] = int64(count)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			currentPods := waitForReplicaSetPods(t, pods, name, count)
			waitForAssociatedPodStates(t, replicaSets, associatedListOptions(caseID, query.Bool(true)), map[string][]map[string]any{
				ns + "/" + name: currentPods,
			})
		}) {
			// Later steps depend on this step's fixture state.
			return
		}
	}
}

func TestDeploymentReplicaSetAssociatedDataRollout(t *testing.T) {
	cl := associatedDataClient(t)
	if _, ok := cl.Schema("apps.deployment"); !ok {
		t.Skip("Deployment schema is not exposed by this target")
	}
	ns := actions.CreateTestNamespace(t, cl)
	deployments := cl.Resource("apps.deployment").Namespace(ns)
	replicaSets := cl.Resource("apps.replicaset").Namespace(ns)
	pods := cl.Resource("pod").Namespace(ns)
	name := "associated-rollout"
	caseID := actions.UniqueName(t, "associated-rollout")
	body := newDeploymentBody(ns, name, map[string]string{actions.CaseLabel: caseID})
	spec := body["spec"].(map[string]any)
	spec["replicas"] = int64(2)
	spec["revisionHistoryLimit"] = int64(2)
	// ReplicaSet metadata inherits the Pod-template labels, not the
	// Deployment's metadata labels. Keep both generations in the query.
	templateMetadata := spec["template"].(map[string]any)["metadata"].(map[string]any)
	templateMetadata["labels"].(map[string]string)[actions.CaseLabel] = caseID
	deployment := actions.CreateAndWait(t, deployments, name, body)
	deploymentUID := nestedStringValue(deployment, "metadata", "uid")
	if deploymentUID == "" {
		t.Fatal("created Deployment has no UID")
	}

	initialSets := waitForDeploymentReplicaSets(t, replicaSets, caseID, deploymentUID, 1)
	oldName := nestedStringValue(initialSets[0], "metadata", "name")
	oldUID := nestedStringValue(initialSets[0], "metadata", "uid")
	initialPods := waitForReplicaSetPods(t, pods, name, 2)
	for _, pod := range initialPods {
		if !lifecycleOwnedBy(pod, "ReplicaSet", oldUID) {
			t.Fatalf("initial Pod %q is not owned by ReplicaSet %q", nestedStringValue(pod, "metadata", "name"), oldName)
		}
	}
	options := associatedListOptions(caseID, query.Bool(true))
	waitForAssociatedPodStates(t, replicaSets, options, map[string][]map[string]any{ns + "/" + oldName: initialPods})
	waitForAssociatedPodStates(t, deployments, options, map[string][]map[string]any{ns + "/" + name: initialPods})

	_, err := actions.UpdateWithConflictRetry(t.Context(), deployments, name, func(resource map[string]any) error {
		currentSpec, ok := resource["spec"].(map[string]any)
		if !ok {
			return fmt.Errorf("Deployment spec has type %T, want object", resource["spec"])
		}
		template, ok := currentSpec["template"].(map[string]any)
		if !ok {
			return fmt.Errorf("Deployment template has type %T, want object", currentSpec["template"])
		}
		metadata, ok := template["metadata"].(map[string]any)
		if !ok {
			return fmt.Errorf("Deployment template metadata has type %T, want object", template["metadata"])
		}
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
			metadata["annotations"] = annotations
		}
		annotations["steve-tests.cattle.io/rollout"] = "second"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	rolledSets := waitForDeploymentReplicaSets(t, replicaSets, caseID, deploymentUID, 2)
	var newName, newUID string
	for _, replicaSet := range rolledSets {
		if nestedStringValue(replicaSet, "metadata", "name") != oldName {
			newName = nestedStringValue(replicaSet, "metadata", "name")
			newUID = nestedStringValue(replicaSet, "metadata", "uid")
		}
	}
	if newName == "" || newUID == "" {
		t.Fatal("rollout did not create a distinct ReplicaSet with a UID")
	}
	// Wait for all old Pods to disappear, rather than taking a snapshot of
	// whichever two Pods happen to exist during the rolling update.
	podValues, err := (query.ListOptions{Filters: []string{query.Equal("metadata.labels.app", name)}}).Values()
	if err != nil {
		t.Fatal(err)
	}
	waitForAssociatedCondition(t, pods, "only the new ReplicaSet's Pods after rollout", func(ctx context.Context) error {
		collection, err := pods.ListValuesContext(ctx, podValues)
		if err != nil {
			return err
		}
		if len(collection.Data) != 2 {
			return fmt.Errorf("Pod count = %d, want 2", len(collection.Data))
		}
		for _, pod := range collection.Data {
			if !lifecycleOwnedBy(pod, "ReplicaSet", newUID) {
				return fmt.Errorf("Pod %q is not owned by the new ReplicaSet %q", nestedStringValue(pod, "metadata", "name"), newName)
			}
		}
		return nil
	})
	rolledPods := waitForReplicaSetPods(t, pods, name, 2)
	waitForAssociatedPodStates(t, replicaSets, options, map[string][]map[string]any{
		ns + "/" + oldName: nil,
		ns + "/" + newName: rolledPods,
	})
	// The existing Deployment association must still contain its new Pods.
	waitForAssociatedPodStates(t, deployments, options, map[string][]map[string]any{ns + "/" + name: rolledPods})
}

func waitForDeploymentReplicaSets(t *testing.T, resources *client.ResourceClient, caseID, deploymentUID string, count int) []map[string]any {
	t.Helper()
	values, err := associatedListOptions(caseID, nil).Values()
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]any
	waitForAssociatedCondition(t, resources, fmt.Sprintf("%d Deployment-owned ReplicaSets", count), func(ctx context.Context) error {
		collection, err := resources.ListValuesContext(ctx, values)
		if err != nil {
			return err
		}
		if len(collection.Data) != count {
			return fmt.Errorf("ReplicaSet count = %d, want %d", len(collection.Data), count)
		}
		for _, resource := range collection.Data {
			if !lifecycleOwnedBy(resource, "Deployment", deploymentUID) {
				return fmt.Errorf("ReplicaSet %q is not owned by the test Deployment", nestedStringValue(resource, "metadata", "name"))
			}
		}
		result = collection.Data
		return nil
	})
	return result
}

func lifecycleOwnedBy(resource map[string]any, kind, uid string) bool {
	if uid == "" {
		return false
	}
	metadata, _ := resource["metadata"].(map[string]any)
	owners, _ := metadata["ownerReferences"].([]any)
	for _, raw := range owners {
		owner, _ := raw.(map[string]any)
		if owner["kind"] == kind && owner["uid"] == uid && owner["controller"] == true {
			return true
		}
	}
	return false
}
