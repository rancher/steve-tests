package deployments

import (
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
	"github.com/rancher/steve-tests/pkg/query"
)

// ReplicaSet associated data was introduced in Steve v0.10.1.
func TestReplicaSetIncludesAssociatedPods(t *testing.T) {
	actions.RequireSteveFeature(t, actions.SteveReplicaSetAssociatedData)
	setup := actions.RequireSQLMode(t)
	actions.RequireWorkloads(t)
	cl := setup.Client
	if _, ok := cl.Schema("apps.replicaset"); !ok {
		t.Skip("ReplicaSet schema is not exposed by this target")
	}
	if _, ok := cl.Schema("pod"); !ok {
		t.Skip("Pod schema is not exposed by this target")
	}

	ns := actions.CreateTestNamespace(t, cl)
	pods := cl.Resource("pod").Namespace(ns)

	replicaSetName := "associated-replicaset"
	replicaSetCase := actions.UniqueName(t, "associated-replicaset")
	replicaSets := cl.Resource("apps.replicaset").Namespace(ns)
	actions.CreateAndWait(t, replicaSets, replicaSetName, newReplicaSetBody(ns, replicaSetName, replicaSetCase))
	replicaSetPod := waitForSinglePod(t, pods, "app", replicaSetName)

	tests := []struct {
		name           string
		include        *bool
		wantAssociated bool
	}{
		{name: "included", include: query.Bool(true), wantAssociated: true},
		{name: "omitted"},
		{name: "explicitly disabled", include: query.Bool(false)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values, err := (query.ListOptions{
				Filters:               []string{query.Equal("metadata.labels."+actions.CaseLabel, replicaSetCase)},
				IncludeAssociatedData: test.include,
			}).Values()
			if err != nil {
				t.Fatal(err)
			}
			if test.wantAssociated {
				seen, names, err := waitForAssociatedPods(replicaSets, values, replicaSetName, []string{replicaSetPod})
				if err != nil {
					t.Fatalf("list ReplicaSet with associated data: %v", err)
				}
				if !seen {
					t.Fatal("ReplicaSet omitted associatedData with includeAssociatedData=true")
				}
				if !reflect.DeepEqual(names, []string{replicaSetPod}) {
					t.Fatalf("associated Pods = %v, want [%s]", names, replicaSetPod)
				}
				return
			}
			collection, err := replicaSets.ListValues(values)
			if err != nil {
				t.Fatal(err)
			}
			if len(collection.Data) != 1 {
				t.Fatalf("ReplicaSet list length = %d, want 1", len(collection.Data))
			}
			metadata, ok := collection.Data[0]["metadata"].(map[string]any)
			if !ok {
				t.Fatalf("ReplicaSet metadata has type %T, want object", collection.Data[0]["metadata"])
			}
			if _, exists := metadata["associatedData"]; exists {
				t.Fatal("associatedData present without includeAssociatedData=true")
			}
		})
	}

}

func newReplicaSetBody(ns string, name string, caseID string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "ReplicaSet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels": actions.ManagedLabels(map[string]string{
				actions.CaseLabel: caseID,
			}),
		},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{
				"matchLabels": map[string]string{"app": name},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]string{"app": name},
				},
				"spec": map[string]any{
					"terminationGracePeriodSeconds": int64(0),
					"containers": []map[string]any{
						{
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

func waitForSinglePod(t *testing.T, pods *client.ResourceClient, label string, value string) string {
	t.Helper()
	values, err := (query.ListOptions{
		Filters: []string{query.Equal("metadata.labels."+label, value)},
		Sort:    []string{"metadata.name"},
	}).Values()
	if err != nil {
		t.Fatalf("encode Pod selector: %v", err)
	}
	deadline := time.Now().Add(pods.WaitTimeout())
	for {
		coll, listErr := pods.ListValues(values)
		if listErr == nil && len(coll.Data) == 1 {
			name := nestedStringValue(coll.Data[0], "metadata", "name")
			if name != "" {
				return name
			}
		}
		if listErr != nil && time.Now().After(deadline) {
			t.Fatalf("wait for controller-created Pod: %v", listErr)
		}
		if time.Now().After(deadline) {
			count := 0
			if coll != nil {
				count = len(coll.Data)
			}
			t.Fatalf("timed out waiting for exactly one Pod with label %s=%s; last count=%d", label, value, count)
		}
		time.Sleep(pods.PollInterval())
	}
}

func waitForAssociatedPods(resources *client.ResourceClient, values url.Values, parent string, want []string) (bool, []string, error) {
	deadline := time.Now().Add(resources.WaitTimeout())
	seen := false
	var last []string
	for {
		coll, err := resources.ListValues(values)
		if err != nil {
			return seen, last, err
		}
		if len(coll.Data) != 1 {
			return seen, last, fmt.Errorf("associated-data query for %s returned %d parents, want 1", parent, len(coll.Data))
		}
		present, names, err := associatedPodNames(coll.Data[0])
		if err != nil {
			return seen, last, err
		}
		seen = seen || present
		last = names
		if present && reflect.DeepEqual(names, want) {
			return true, names, nil
		}
		if time.Now().After(deadline) {
			return seen, last, nil
		}
		time.Sleep(resources.PollInterval())
	}
}

func associatedPodNames(resource map[string]any) (bool, []string, error) {
	metadata, ok := resource["metadata"].(map[string]any)
	if !ok {
		return false, nil, fmt.Errorf("resource metadata has type %T, want object", resource["metadata"])
	}
	raw, present := metadata["associatedData"]
	if !present {
		return false, nil, nil
	}
	blocks, ok := raw.([]any)
	if !ok {
		return true, nil, fmt.Errorf("metadata.associatedData has type %T, want array", raw)
	}
	var names []string
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			return true, nil, fmt.Errorf("associated-data block has type %T, want object", rawBlock)
		}
		gvk, _ := block["gvk"].(map[string]any)
		if gvk["kind"] != "Pod" {
			continue
		}
		children, ok := block["data"].([]any)
		if !ok {
			return true, nil, fmt.Errorf("Pod associated-data block has data type %T, want array", block["data"])
		}
		for _, rawChild := range children {
			child, ok := rawChild.(map[string]any)
			if !ok {
				return true, nil, fmt.Errorf("associated child has type %T, want object", rawChild)
			}
			name, _ := child["childName"].(string)
			if name != "" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return true, names, nil
}

func nestedStringValue(resource map[string]any, keys ...string) string {
	var current any = resource
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	value, _ := current.(string)
	return value
}
