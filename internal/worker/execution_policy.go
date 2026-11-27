package worker

import "errors"

const (
	// ExecutionTrustedLocal retains the existing operator-approved subprocess
	// path. An empty policy has the same meaning for older configurations.
	ExecutionTrustedLocal = "trusted-local.v1"
	// ExecutionUntrustedIsolated requires an execution boundary that does not
	// expose the trusted Worker's credentials or state to customer code.
	ExecutionUntrustedIsolated = "untrusted-isolated.v1"
	// KubernetesGVisorProvider delegates native work to the fixed control-plane
	// Pod controller; the trusted Worker only validates and publishes bytes.
	KubernetesGVisorProvider = "kubernetes-gvisor.v1"
)

// ErrIsolationUnavailable prevents task execution from falling back to a local
// subprocess while no isolated execution provider is implemented.
var ErrIsolationUnavailable = errors.New("untrusted task execution requires an isolated provider; no isolated provider is available")
