package indexedfields

import "fmt"

// HPAs target a nonexistent Deployment, so these fixtures create no workloads.
func hpa(minReplicas, maxReplicas int) map[string]any {
	return map[string]any{
		"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler",
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "steve-test-nonexistent"},
			"minReplicas":    minReplicas, "maxReplicas": maxReplicas,
			"metrics": []any{map[string]any{
				"type": "Resource",
				"resource": map[string]any{
					"name":   "cpu",
					"target": map[string]any{"type": "Utilization", "averageUtilization": 50},
				},
			}},
		},
	}
}

func configMap(keys int) map[string]any {
	data := make(map[string]string, keys)
	for i := 0; i < keys; i++ {
		data[fmt.Sprintf("key-%02d", i)] = "value"
	}
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "data": data}
}

const (
	crdResource           = "apiextensions.k8s.io.customresourcedefinition"
	descriptionAnnotation = "field.cattle.io/description"
	descriptionField      = "metadata.annotations[" + descriptionAnnotation + "]"
)

func customResourceDefinition(plural, group, singular string) map[string]any {
	return map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group": group, "scope": "Namespaced",
			"names": map[string]any{"plural": plural, "singular": singular, "kind": "Indexed" + plural},
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object"}},
			}},
		},
	}
}

func withDescription(body map[string]any, description string) map[string]any {
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any)
		body["metadata"] = metadata
	}
	metadata["annotations"] = map[string]any{descriptionAnnotation: description}
	return body
}
