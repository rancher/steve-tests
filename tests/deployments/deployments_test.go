package deployments

import (
	"testing"

	"github.com/rancher/steve-tests/actions"
	"github.com/rancher/steve-tests/pkg/client"
)

func getTestClient(t *testing.T) *client.Client {
	return actions.IntegrationClient(t)
}

// newDeploymentBody creates an apps/v1 Deployment with replicas=0 so the
// scenarios only depend on Steve list behavior, never on image pulls or
// scheduling. Deployments exercise the non-core group path: the schema ID is
// apps.deployment and the collection lives at /v1/apps.deployments.
func newDeploymentBody(ns string, name string, labels map[string]string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    actions.ManagedLabels(labels),
		},
		"spec": map[string]any{
			"replicas": int64(0),
			"selector": map[string]any{
				"matchLabels": map[string]string{
					"app": name,
				},
			},
			"template": newWorkloadPodTemplate(name),
		},
	}
}

// newWorkloadPodTemplate supplies the shared disposable Pod used by workload fixtures.
func newWorkloadPodTemplate(name string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				"app": name,
			},
		},
		"spec": map[string]any{
			// Disposable fixtures need no graceful shutdown delay.
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
	}
}
