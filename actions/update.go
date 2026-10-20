package actions

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rancher/steve-tests/pkg/client"
)

// UpdateWithConflictRetry reads the current resource, applies mutate, and sends
// it back through PUT. Each conflict starts from a fresh GET so controller
// changes and resourceVersion are preserved. mutate may run more than once.
// Only 409 responses are retried, within the resource's configured wait timeout.
func UpdateWithConflictRetry(
	ctx context.Context,
	resources *client.ResourceClient,
	name string,
	mutate func(map[string]any) error,
) (map[string]any, error) {
	if mutate == nil {
		return nil, fmt.Errorf("update %q: mutation function is nil", name)
	}
	ctx, cancel := context.WithTimeout(ctx, waitTimeout(resources))
	defer cancel()

	var lastConflict error
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("update %q: %w (last conflict: %v)", name, err, lastConflict)
		}
		current, err := resources.GetContext(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("get %q before update: %w", name, err)
		}
		metadata, ok := current["metadata"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("update %q: resource has no metadata object", name)
		}
		resourceVersion, ok := metadata["resourceVersion"].(string)
		if !ok || resourceVersion == "" {
			return nil, fmt.Errorf("update %q: resource has no resourceVersion", name)
		}
		if err := mutate(current); err != nil {
			return nil, fmt.Errorf("prepare update of %q: %w", name, err)
		}
		if getNestedString(current, "metadata", "resourceVersion") != resourceVersion {
			return nil, fmt.Errorf("update %q: mutation changed resourceVersion", name)
		}
		updated, err := resources.UpdateContext(ctx, name, current)
		if err == nil {
			return updated, nil
		}
		if !client.IsStatus(err, http.StatusConflict) {
			return nil, fmt.Errorf("update %q: %w", name, err)
		}
		lastConflict = err
		interval := resources.PollInterval()
		if interval <= 0 {
			interval = 200 * time.Millisecond
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("update %q: %w (last conflict: %v)", name, ctx.Err(), lastConflict)
		case <-timer.C:
		}
	}
}
