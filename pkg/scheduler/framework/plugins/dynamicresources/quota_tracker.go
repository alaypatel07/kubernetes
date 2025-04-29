/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dynamicresources

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	quotacore "k8s.io/kubernetes/pkg/quota/v1/evaluator/core"
)

// QuotaTracker tracks device quota usage at allocation time.
type QuotaTracker struct {
	// lock protects all fields
	lock sync.RWMutex

	// usagePerNamespace tracks device usage by namespace and device class
	usagePerNamespace map[string]map[corev1.ResourceName]*resource.Quantity

	// quotasPerNamespace tracks quota limits by namespace and device class
	quotasPerNamespace map[string]map[corev1.ResourceName]*resource.Quantity

	// claimAllocations tracks the allocation results per claim to enable deallocation during cleanup
	claimAllocations map[types.UID]resourceapi.AllocationResult
}

// NewQuotaTracker creates a new QuotaTracker.
func NewQuotaTracker() *QuotaTracker {
	return &QuotaTracker{
		usagePerNamespace:  make(map[string]map[corev1.ResourceName]*resource.Quantity),
		quotasPerNamespace: make(map[string]map[corev1.ResourceName]*resource.Quantity),
		claimAllocations:   make(map[types.UID]resourceapi.AllocationResult),
	}
}

// DeviceUsageFromAllocation calculates the device quota usage from an allocation result.
func DeviceUsageFromAllocation(allocation *resourceapi.AllocationResult) map[corev1.ResourceName]*resource.Quantity {
	usage := make(map[corev1.ResourceName]*resource.Quantity)

	if allocation == nil || allocation.Devices.Results == nil {
		return usage
	}

	// Count devices by device class
	deviceCountByClass := make(map[string]int64)
	for _, result := range allocation.Devices.Results {
		// Extract the device class name from the allocation result
		// Assume we can extract it from the device's driver and pool information
		deviceClass := result.Request
		deviceCountByClass[deviceClass]++
	}

	// Convert to resource quantities
	for deviceClass, count := range deviceCountByClass {
		resourceName := quotacore.V1ResourceByDeviceClass(deviceClass)
		quantity := resource.NewQuantity(count, resource.DecimalSI)
		usage[resourceName] = quantity
	}

	return usage
}

// CanAllocate checks if allocation is possible given current namespace quotas.
func (t *QuotaTracker) CanAllocate(ctx context.Context, namespace string, allocation *resourceapi.AllocationResult) error {
	t.lock.RLock()
	defer t.lock.RUnlock()
	logger := klog.FromContext(ctx)

	usage := DeviceUsageFromAllocation(allocation)
	if len(usage) == 0 {
		return nil // No device allocation, nothing to check
	}

	logger.V(2).Info("Usage from allocation", "namespace", namespace, "allocation", allocation, "usage", usage)

	// Get current namespace usage
	nsUsage, nsExists := t.usagePerNamespace[namespace]
	nsQuotas, quotasExist := t.quotasPerNamespace[namespace]

	// If no quotas exist for this namespace, allow the allocation
	if !quotasExist {
		return nil
	}

	// Check each device class against its quota
	for resourceName, requestedQuantity := range usage {
		quota, hasQuota := nsQuotas[resourceName]
		if !hasQuota {
			continue // No quota for this resource, allow
		}

		// Calculate current usage + requested
		currentUsage := resource.NewQuantity(0, resource.DecimalSI)
		if nsExists {
			if existing, exists := nsUsage[resourceName]; exists {
				c := existing.DeepCopy()
				currentUsage = &c
			}
		}

		currentUsage.Add(*requestedQuantity)

		// Check if exceeds quota
		if currentUsage.Cmp(*quota) > 0 {
			temp := quota.DeepCopy()
			temp.Sub(*currentUsage)
			return fmt.Errorf("allocation would exceed quota for resource %s in namespace %s (requested: %v, available: %v)",
				resourceName, namespace, requestedQuantity.String(), temp)
		}
	}

	return nil
}

// RecordAllocation records a successful allocation for quota tracking.
func (t *QuotaTracker) RecordAllocation(ctx context.Context, clientset kubernetes.Interface, namespace string, claimUID types.UID, allocation *resourceapi.AllocationResult) {
	t.lock.Lock()
	defer t.lock.Unlock()
	logger := klog.FromContext(ctx)

	usage := DeviceUsageFromAllocation(allocation)
	if len(usage) == 0 {
		return // No device allocation, nothing to track
	}

	// Get or create namespace usage map
	nsUsage, exists := t.usagePerNamespace[namespace]
	if !exists {
		nsUsage = make(map[corev1.ResourceName]*resource.Quantity)
		t.usagePerNamespace[namespace] = nsUsage
	}

	// Update usage for each resource
	for resourceName, quantity := range usage {
		current, exists := nsUsage[resourceName]
		if !exists {
			q := quantity.DeepCopy()
			nsUsage[resourceName] = &q
		} else {
			current.Add(*quantity)
		}
	}

	// Track the allocation for potential cleanup later
	t.claimAllocations[claimUID] = *allocation

	// Update ResourceQuota status
	if err := t.UpdateResourceQuotaStatus(ctx, clientset, namespace); err != nil {
		logger.Error(err, "Failed to update ResourceQuota status", "namespace", namespace)
		// Don't fail scheduling if we can't update quota status
	}
}

// RemoveAllocation removes an allocation when a claim is deleted or deallocated.
func (t *QuotaTracker) RemoveAllocation(ctx context.Context, clientset kubernetes.Interface, namespace string, claimUID types.UID) {
	t.lock.Lock()
	defer t.lock.Unlock()

	allocation, exists := t.claimAllocations[claimUID]
	if !exists {
		return // Nothing to remove
	}

	usage := DeviceUsageFromAllocation(&allocation)
	if len(usage) == 0 {
		delete(t.claimAllocations, claimUID)
		return // No device allocation, nothing to track
	}

	// Get namespace usage map
	nsUsage, exists := t.usagePerNamespace[namespace]
	if !exists {
		// This shouldn't happen but is not critical
		delete(t.claimAllocations, claimUID)
		return
	}

	// Update usage for each resource
	for resourceName, quantity := range usage {
		current, exists := nsUsage[resourceName]
		if !exists {
			// This shouldn't happen but is not critical
			continue
		}

		current.Sub(*quantity)

		// If quantity is zero or negative, remove it
		if current.Sign() <= 0 {
			delete(nsUsage, resourceName)
		}
	}

	// Cleanup empty namespace entries
	if len(nsUsage) == 0 {
		delete(t.usagePerNamespace, namespace)
	}

	// Remove the claim allocation tracking
	delete(t.claimAllocations, claimUID)

	// Update ResourceQuota status
	if err := t.UpdateResourceQuotaStatus(ctx, clientset, namespace); err != nil {
		klog.FromContext(ctx).Error(err, "Failed to update ResourceQuota status", "namespace", namespace)
	}
}

// UpdateQuotas updates the quota limits for a namespace.
func (t *QuotaTracker) UpdateQuotas(namespace string, quotaList corev1.ResourceQuotaList) {
	t.lock.Lock()
	defer t.lock.Unlock()

	// Create new quota map for the namespace
	nsQuotas := make(map[corev1.ResourceName]*resource.Quantity)

	// Process all quotas
	for _, quota := range quotaList.Items {
		if quota.Spec.Hard == nil {
			continue
		}

		for resourceName, hard := range quota.Spec.Hard {
			// Check if this is a device class resource
			if !isDRADeviceQuota(resourceName) {
				continue
			}

			current, exists := nsQuotas[resourceName]
			if !exists || hard.Cmp(*current) < 0 {
				// Use the smaller quota limit if multiple exist
				q := hard.DeepCopy()
				nsQuotas[resourceName] = &q
			}
		}
	}

	// Update the namespace quotas
	if len(nsQuotas) > 0 {
		t.quotasPerNamespace[namespace] = nsQuotas
	} else {
		delete(t.quotasPerNamespace, namespace)
	}
}

// isDRADeviceQuota checks if a resource name represents a DRA device quota.
func isDRADeviceQuota(name corev1.ResourceName) bool {
	return len(name) > 0 && name != quotacore.ClaimObjectCountName &&
		name != corev1.ResourcePods && name != corev1.ResourceServices &&
		name != corev1.ResourceConfigMaps && name != corev1.ResourceSecrets &&
		name != corev1.ResourcePersistentVolumeClaims &&
		name != corev1.ResourceCPU && name != corev1.ResourceMemory
}

// GetUsage returns the current quota usage for a namespace.
func (t *QuotaTracker) GetUsage(namespace string) map[corev1.ResourceName]*resource.Quantity {
	t.lock.RLock()
	defer t.lock.RUnlock()

	nsUsage, exists := t.usagePerNamespace[namespace]
	if !exists {
		return make(map[corev1.ResourceName]*resource.Quantity)
	}

	// Make a deep copy to avoid concurrent access issues
	result := make(map[corev1.ResourceName]*resource.Quantity, len(nsUsage))
	for name, quantity := range nsUsage {
		q := quantity.DeepCopy()
		result[name] = &q
	}

	return result
}

// UpdateResourceQuotaStatus updates the ResourceQuota status to reflect current allocation usage
func (t *QuotaTracker) UpdateResourceQuotaStatus(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	t.lock.RLock()
	defer t.lock.RUnlock()

	// Get current usage for namespace
	nsUsage, exists := t.usagePerNamespace[namespace]
	if !exists {
		return nil
	}

	// Get all ResourceQuotas in namespace
	quotaList, err := clientset.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list ResourceQuotas: %w", err)
	}

	// Update status of each ResourceQuota
	for _, quota := range quotaList.Items {
		statusUpdated := false
		quotaCopy := quota.DeepCopy()

		if quotaCopy.Status.Used == nil {
			quotaCopy.Status.Used = corev1.ResourceList{}
		}

		// Update each tracked resource's usage
		for resourceName, usage := range nsUsage {
			if _, exists := quotaCopy.Spec.Hard[resourceName]; exists {
				statusUpdated = true
				quotaCopy.Status.Used[resourceName] = usage.DeepCopy()
			}
		}

		// Only update if there were changes
		if statusUpdated {
			_, err = clientset.CoreV1().ResourceQuotas(namespace).UpdateStatus(ctx, quotaCopy, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update ResourceQuota status for %s: %w", quota.Name, err)
			}
		}
	}

	return nil
}
