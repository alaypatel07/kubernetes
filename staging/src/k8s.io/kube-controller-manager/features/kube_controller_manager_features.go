package features

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"
)

const (
	// Every feature gate should add method here following this template:
	//
	// // owner: @username
	// // alpha: v1.4
	// MyFeature() bool

	// owner: @alaypatel07, @soltysh
	// alpha: v1.20
	// beta: v1.21
	//
	// CronjobControllerV2 controls whether the controller manager starts old cronjob
	// controller or new one which is implemented with informers and delaying queue
	//
	// This feature is deprecated, and will be removed in v1.22.
	CronjobControllerV2 featuregate.Feature = "CronjobControllerV2"
)

var DefaultMutableFeatureGate featuregate.MutableFeatureGate = featuregate.NewFeatureGate()

func init() {
	runtime.Must(DefaultMutableFeatureGate.Add(defaultControllerManagerFeatureGates))
}

// defaultControllerManagerFeatureGates consists of all known Kubernetes-specific feature keys.
// To add a new feature, define a key for it above and add it here. The features will be
// available throughout Kubernetes binaries.
var defaultControllerManagerFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	CronjobControllerV2: {Default: false, PreRelease: featuregate.Alpha},
}
