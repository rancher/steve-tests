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

// ReplicaSet associated data was introduced in Steve v0.10.1 (rancher/rancher#56210).
func TestReplicaSetIncludesAssociatedPods(t *testing.T) {
	cl := associatedDataClient(t)
	ns := actions.CreateTestNamespace(t, cl)
	resources := cl.Resource("apps.replicaset").Namespace(ns)
	caseID := actions.UniqueName(t, "associated-content")
	name := "associated-replicaset"
	body := newReplicaSetBody(ns, name, caseID)
	body["spec"].(map[string]any)["replicas"] = int64(2)
	actions.CreateAndWait(t, resources, name, body)
	pods := waitForReplicaSetPods(t, cl.Resource("pod").Namespace(ns), name, 2)

	for _, tc := range []struct {
		name    string
		include *bool
		want    []map[string]any
	}{
		{"included", query.Bool(true), pods},
		{"omitted", nil, nil},
		{"explicitly disabled", query.Bool(false), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitForAssociatedPodStates(t, resources, associatedListOptions(caseID, tc.include),
				map[string][]map[string]any{ns + "/" + name: tc.want})
		})
	}
}

func TestReplicaSetAssociatedDataIsolation(t *testing.T) {
	cl := associatedDataClient(t)
	ns := actions.CreateTestNamespace(t, cl)
	otherNS := actions.CreateTestNamespace(t, cl)
	caseID := actions.UniqueName(t, "associated-isolation")
	name := "same-name"
	expected := make(map[string][]map[string]any)
	for _, namespace := range []string{ns, otherNS} {
		body := newReplicaSetBody(namespace, name, caseID)
		spec := body["spec"].(map[string]any)
		spec["selector"].(map[string]any)["matchLabels"].(map[string]string)["tier"] = "wanted"
		spec["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]string)["tier"] = "wanted"
		actions.CreateAndWait(t, cl.Resource("apps.replicaset").Namespace(namespace), name, body)
		expected[namespace+"/"+name] = waitForReplicaSetPods(t, cl.Resource("pod").Namespace(namespace), name, 1)
	}

	// A partial selector match in the same namespace must not be associated.
	// A different owner alone is not a negative control: Steve matches labels.
	template := newReplicaSetBody(ns, "decoy", caseID)["spec"].(map[string]any)["template"].(map[string]any)
	decoy := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "decoy", "namespace": ns,
			"labels": actions.ManagedLabels(map[string]string{"app": name, "tier": "other"})},
		"spec": template["spec"],
	}
	actions.CreateAndWait(t, cl.Resource("pod").Namespace(ns), "decoy", decoy)
	opts := associatedListOptions(caseID, query.Bool(true))
	t.Run("namespaced", func(t *testing.T) {
		waitForAssociatedPodStates(t, cl.Resource("apps.replicaset").Namespace(ns), opts,
			map[string][]map[string]any{ns + "/" + name: expected[ns+"/"+name]})
	})
	t.Run("cluster-wide", func(t *testing.T) {
		waitForAssociatedPodStates(t, cl.Resource("apps.replicaset"), opts, expected)
	})
}

func TestReplicaSetAssociatedDataPagination(t *testing.T) {
	cl := associatedDataClient(t)
	ns := actions.CreateTestNamespace(t, cl)
	resources := cl.Resource("apps.replicaset").Namespace(ns)
	caseID := actions.UniqueName(t, "associated-pages")
	names := []string{"page-alpha", "page-beta", "page-gamma"}
	expected := make(map[string][]map[string]any)
	for i, name := range names {
		body := newReplicaSetBody(ns, name, caseID)
		body["spec"].(map[string]any)["replicas"] = int64(i + 1)
		actions.CreateAndWait(t, resources, name, body)
		expected[ns+"/"+name] = waitForReplicaSetPods(t, cl.Resource("pod").Namespace(ns), name, i+1)
	}
	opts := associatedListOptions(caseID, query.Bool(true))
	waitForAssociatedPodStates(t, resources, opts, expected)
	for _, size := range []int{1, 2} {
		t.Run(fmt.Sprintf("pagesize=%d", size), func(t *testing.T) {
			waitForAssociatedCondition(t, resources, "complete paginated associated data", func(ctx context.Context) error {
				revision := ""
				var walked []string
				for offset := 0; offset < len(names); offset += size {
					pageOpts := opts
					pageOpts.Sort = []string{"metadata.name"}
					pageOpts.PageSize = size
					pageOpts.Page = offset/size + 1
					pageOpts.Revision = revision
					values, err := pageOpts.Values()
					if err != nil {
						return err
					}
					coll, err := resources.ListValuesContext(ctx, values)
					if err != nil {
						return err
					}
					if revision == "" {
						revision = coll.Revision
					}
					if coll.Count != len(names) {
						return fmt.Errorf("page %d total count = %d, want %d", pageOpts.Page, coll.Count, len(names))
					}
					end := min(offset+size, len(names))
					pageWant := make(map[string][]map[string]any)
					for _, name := range names[offset:end] {
						pageWant[ns+"/"+name] = expected[ns+"/"+name]
					}
					if err := checkAssociatedPodStates(coll.Data, pageWant); err != nil {
						return fmt.Errorf("page %d: %w", pageOpts.Page, err)
					}
					for _, item := range coll.Data {
						walked = append(walked, nestedStringValue(item, "metadata", "name"))
					}
				}
				if !reflect.DeepEqual(walked, names) {
					return fmt.Errorf("walked parents = %v, want %v", walked, names)
				}
				return nil
			})
		})
	}
}

func associatedDataClient(t *testing.T) *client.Client {
	t.Helper()
	actions.RequireSteveFeature(t, actions.SteveReplicaSetAssociatedData)
	setup := actions.RequireSQLMode(t)
	actions.RequireWorkloads(t)
	for _, kind := range []string{"apps.replicaset", "pod"} {
		if _, ok := setup.Client.Schema(kind); !ok {
			t.Skipf("%s schema is not exposed by this target", kind)
		}
	}
	return setup.Client
}

func associatedListOptions(caseID string, include *bool) query.ListOptions {
	return query.ListOptions{
		Filters:               []string{query.Equal("metadata.labels."+actions.CaseLabel, caseID)},
		IncludeAssociatedData: include,
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
			"template": newWorkloadPodTemplate(name),
		},
	}
}

// All association state fields are strings, unlike direct Pod boolean flags.
type associatedPodState struct {
	Name, Error, Message, Transitioning string
}

func associatedPodStates(resource map[string]any) (map[string]associatedPodState, error) {
	metadata, ok := resource["metadata"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("metadata has type %T, want object", resource["metadata"])
	}
	raw, exists := metadata["associatedData"]
	if !exists {
		return nil, nil
	}
	blocks, ok := raw.([]any)
	if !ok || len(blocks) != 1 {
		return nil, fmt.Errorf("associatedData = %#v, want exactly one Pod block", raw)
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("associated block has type %T, want object", blocks[0])
	}
	wantGVK := map[string]any{"group": "", "version": "v1", "kind": "Pod"}
	if !reflect.DeepEqual(block["gvk"], wantGVK) {
		return nil, fmt.Errorf("associated GVK = %#v, want %#v", block["gvk"], wantGVK)
	}
	children, ok := block["data"].([]any)
	if !ok || len(children) == 0 {
		return nil, fmt.Errorf("Pod associated data = %#v, want nonempty array", block["data"])
	}
	result := make(map[string]associatedPodState, len(children))
	for _, rawChild := range children {
		child, ok := rawChild.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("associated child has type %T, want object", rawChild)
		}
		name, ok := child["childName"].(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid childName: %#v", child["childName"])
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate associated child %q", name)
		}
		state, ok := child["state"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("child %q state has type %T, want object", name, child["state"])
		}
		values := make([]string, 4)
		for i, key := range []string{"name", "error", "message", "transitioning"} {
			value, ok := state[key].(string)
			if !ok {
				return nil, fmt.Errorf("child %q state.%s has type %T, want string", name, key, state[key])
			}
			values[i] = value
		}
		result[name] = associatedPodState{values[0], values[1], values[2], values[3]}
	}
	return result, nil
}

func directPodStates(pods []map[string]any) (map[string]associatedPodState, error) {
	if len(pods) == 0 {
		return nil, nil
	}
	result := make(map[string]associatedPodState, len(pods))
	for _, pod := range pods {
		name := nestedStringValue(pod, "metadata", "name")
		metadata, _ := pod["metadata"].(map[string]any)
		state, ok := metadata["state"].(map[string]any)
		if name == "" || !ok || nestedStringValue(pod, "metadata", "state", "name") == "" {
			return nil, fmt.Errorf("direct Pod %q has no name/state: %#v", name, state)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate expected Pod %q", name)
		}
		values := make([]string, 4)
		for i, key := range []string{"name", "error", "message", "transitioning"} {
			// Steve's SQL cache normalizes nil to empty string and scalar values
			// with fmt.Sprint; false must become "false", never an empty string.
			switch value := state[key].(type) {
			case nil:
				values[i] = ""
			case string:
				values[i] = value
			case bool:
				values[i] = fmt.Sprint(value)
			default:
				return nil, fmt.Errorf("direct Pod %q state.%s has unexpected type %T", name, key, value)
			}
		}
		result[name] = associatedPodState{values[0], values[1], values[2], values[3]}
	}
	return result, nil
}

func checkAssociatedPodStates(items []map[string]any, want map[string][]map[string]any) error {
	if len(items) != len(want) {
		return fmt.Errorf("returned %d parents, want %d", len(items), len(want))
	}
	seen := make(map[string]bool)
	for _, item := range items {
		id := nestedStringValue(item, "metadata", "namespace") + "/" + nestedStringValue(item, "metadata", "name")
		pods, exists := want[id]
		if !exists || seen[id] {
			return fmt.Errorf("unexpected or duplicate parent %q", id)
		}
		seen[id] = true
		expected, err := directPodStates(pods)
		if err != nil {
			return err
		}
		got, err := associatedPodStates(item)
		if err != nil {
			return fmt.Errorf("parent %s: %w", id, err)
		}
		if !reflect.DeepEqual(got, expected) {
			return fmt.Errorf("parent %s associated Pods = %#v, want %#v", id, got, expected)
		}
	}
	return nil
}

func waitForAssociatedPodStates(t *testing.T, resources *client.ResourceClient, opts query.ListOptions, want map[string][]map[string]any) {
	t.Helper()
	values, err := opts.Values()
	if err != nil {
		t.Fatal(err)
	}
	waitForAssociatedCondition(t, resources, "exact associated Pod names and states", func(ctx context.Context) error {
		coll, err := resources.ListValuesContext(ctx, values)
		if err != nil {
			return err
		}
		return checkAssociatedPodStates(coll.Data, want)
	})
}

func waitForReplicaSetPods(t *testing.T, pods *client.ResourceClient, app string, count int) []map[string]any {
	t.Helper()
	values, err := (query.ListOptions{Filters: []string{query.Equal("metadata.labels.app", app)}}).Values()
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]any
	waitForAssociatedCondition(t, pods, fmt.Sprintf("%d ready Pods for app=%s", count, app), func(ctx context.Context) error {
		coll, err := pods.ListValuesContext(ctx, values)
		if err != nil {
			return err
		}
		if len(coll.Data) != count {
			return fmt.Errorf("Pod count = %d, want %d", len(coll.Data), count)
		}
		current := make([]map[string]any, 0, count)
		for _, item := range coll.Data {
			name := nestedStringValue(item, "metadata", "name")
			pod, err := pods.GetContext(ctx, name)
			if err != nil {
				return err
			}
			if nestedStringValue(pod, "metadata", "deletionTimestamp") != "" {
				return fmt.Errorf("Pod %s is still terminating", name)
			}
			if !associatedPodHasReadiness(pod, true) || nestedStringValue(pod, "metadata", "state", "name") != "running" {
				return fmt.Errorf("Pod %s is not ready/running: %#v", name, pod["status"])
			}
			current = append(current, pod)
		}
		result = current
		return nil
	})
	return result
}

// Missing/unknown readiness must not satisfy an expected Ready=False condition.
func associatedPodHasReadiness(pod map[string]any, ready bool) bool {
	want := "False"
	if ready {
		want = "True"
	}
	status, _ := pod["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "Ready" {
			return condition["status"] == want
		}
	}
	return false
}

func waitForAssociatedCondition(t *testing.T, resources *client.ResourceClient, description string, check func(context.Context) error) {
	t.Helper()
	waitForAssociatedConditionTimeout(t, resources, resources.WaitTimeout(), description, check)
}

func waitForAssociatedConditionTimeout(t *testing.T, resources *client.ResourceClient, timeout time.Duration, description string, check func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var last error
	for {
		if ctx.Err() != nil {
			t.Fatalf("waiting for %s: %v; last result: %v", description, ctx.Err(), last)
		}
		last = check(ctx)
		if last == nil {
			return
		}
		timer := time.NewTimer(resources.PollInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("waiting for %s: %v; last result: %v", description, ctx.Err(), last)
		case <-timer.C:
		}
	}
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
