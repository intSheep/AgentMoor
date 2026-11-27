package sandboxkube

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"time"

	"agentmoor/internal/asset"
	"agentmoor/internal/platform"
)

var (
	ErrRun              = errors.New("sandbox execution cannot proceed")
	ErrMissingPod       = errors.New("sandbox create was not observed; recreation is forbidden")
	ErrGuestStopUnknown = errors.New("sandbox guest stop is not confirmed")
)

const cleanupTimeout = 5 * time.Second

type Controller struct {
	profiles map[asset.Scope]Profile
	client   *Client
	catalog  *platform.Catalog
	// Bounded local serialization prevents a concurrent observer mistaking the
	// fence-to-POST interval for a missing Pod. The durable fence, not this lock,
	// prevents another POST across process restarts and uncertain responses.
	locks [64]sync.Mutex
}

func New(config ClientConfig, profiles []Profile, catalog *platform.Catalog) (*Controller, error) {
	if catalog == nil || len(profiles) == 0 || len(profiles) > 1024 {
		return nil, ErrProfile
	}
	c := &Controller{profiles: make(map[asset.Scope]Profile, len(profiles)), catalog: catalog}
	for _, p := range profiles {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := c.profiles[p.Scope]; duplicate {
			return nil, ErrProfile
		}
		c.profiles[p.Scope] = p
	}
	var err error
	c.client, err = NewClient(config)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Controller) Profile(scope asset.Scope) (Profile, bool) {
	p, ok := c.profiles[scope]
	return p, ok
}

func (c *Controller) lock(ctx context.Context, a platform.SandboxAddress) (func(), error) {
	h := fnv.New32a()
	h.Write([]byte(a.Scope.Prefix() + "/" + a.TurnID + "/" + a.Attempt))
	m := &c.locks[h.Sum32()%uint32(len(c.locks))]
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.TryLock() {
		return m.Unlock, nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if m.TryLock() {
				return m.Unlock, nil
			}
		}
	}
}

// Load persisted ownership even when the real task has since been cancelled,
// expired or changed. Current task authority is checked separately before use.
func (c *Controller) load(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	if _, ok := c.profiles[a.Scope]; !ok {
		return platform.SandboxRun{}, ErrProfile
	}
	runs, err := c.catalog.ListSandboxRuns(ctx, a.Scope)
	if err != nil {
		return platform.SandboxRun{}, err
	}
	for _, r := range runs {
		if r.Address() == a {
			return r, nil
		}
	}
	return platform.SandboxRun{}, platform.ErrMissing
}

func stopped(r platform.SandboxRun) bool {
	return r.Collected || r.State == platform.SandboxReady || r.State == platform.SandboxFailed ||
		r.State == platform.SandboxCancelled || r.State == platform.SandboxExpired
}

// Start consumes at most one committed create permit. Retrying this method is
// safe: an earlier fence always takes the GET-only recovery path.
func (c *Controller) Start(ctx context.Context, a platform.SandboxAddress) (result platform.SandboxRun, err error) {
	unlock, err := c.lock(ctx, a)
	if err != nil {
		return result, err
	}
	defer unlock()
	defer func() {
		if ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			if r, e := c.catalog.CancelSandbox(cleanup, a); e == nil {
				result = r
			}
			if r, e := c.reconcile(cleanup, a); e == nil {
				result = r
			}
			err = ctx.Err()
		}
	}()
	r, err := c.load(ctx, a)
	if err != nil {
		return r, err
	}
	p := c.profiles[a.Scope]
	hash, _ := p.Fingerprint()
	if r.ProfileFingerprint != hash || stopped(r) || r.CreateStarted {
		return c.reconcile(ctx, a)
	}
	permit, err := c.catalog.FenceSandboxCreate(ctx, a, r.Deadline)
	if err != nil {
		return r, err
	}
	if !permit.CreateAllowed {
		return c.reconcile(ctx, a)
	}
	r = permit.Run
	remaining := time.Until(r.Deadline)
	seconds := int64((remaining + time.Second - 1) / time.Second)
	body, templateHash, err := podTemplate(p, r, permit.Capabilities, seconds)
	if err != nil {
		c.fail(ctx, a, "invalid-template")
		return r, err
	}
	r, err = c.catalog.SealSandboxPodTemplate(ctx, a, p.Namespace, templateHash)
	if err != nil {
		return r, err
	}
	// Create is intentionally never retried, including 409/5xx/timeouts. The
	// only next operation is observing this already-fenced name.
	_, createErr := c.client.Create(ctx, p.Namespace, body)
	result, err = c.reconcile(ctx, a)
	if err != nil {
		return result, err
	}
	if result.State == platform.SandboxFailed && createErr != nil {
		return result, ErrMissingPod
	}
	return result, nil
}

func (c *Controller) fail(ctx context.Context, a platform.SandboxAddress, code string) (platform.SandboxRun, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	r, err := c.load(cleanup, a)
	if err != nil || r.Collected {
		return r, err
	}
	return c.catalog.FailSandbox(cleanup, a, code)
}

// Cancel revokes delegated authority before waiting for a local in-flight
// Kubernetes operation. Cleanup is bounded and survives caller cancellation.
func (c *Controller) Cancel(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	r, err := c.load(cleanup, a)
	if err != nil {
		return r, err
	}
	if !r.Collected {
		if _, err := c.catalog.CancelSandbox(cleanup, a); err != nil {
			return r, err
		}
	}
	return c.Reconcile(cleanup, a)
}

func (c *Controller) Reconcile(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	unlock, err := c.lock(ctx, a)
	if err != nil {
		return platform.SandboxRun{}, err
	}
	defer unlock()
	return c.reconcile(ctx, a)
}

func (c *Controller) reconcile(ctx context.Context, a platform.SandboxAddress) (platform.SandboxRun, error) {
	r, err := c.load(ctx, a)
	if err != nil {
		return r, err
	}
	hash, _ := c.profiles[a.Scope].Fingerprint()
	if !stopped(r) {
		current, check := c.catalog.SandboxRun(ctx, a)
		switch {
		case hash != r.ProfileFingerprint:
			r, err = c.fail(ctx, a, "profile-changed")
		case check != nil:
			if !errors.Is(check, platform.ErrMissing) && !errors.Is(check, platform.ErrConflict) {
				return r, check
			}
			r, err = c.fail(ctx, a, "claim-changed")
		case !time.Now().Before(r.Deadline):
			r, err = c.fail(ctx, a, "deadline-exceeded")
		case current.State == platform.SandboxCancelled:
			r, err = c.catalog.CancelSandbox(ctx, a)
		case stopped(current):
			r, err = c.fail(ctx, a, "claim-stopped")
		}
		if err != nil {
			return r, err
		}
	}
	if !r.CreateStarted || r.PodDeleted {
		return r, nil
	}
	if r.PodTemplateSHA == "" || r.PodNamespace == "" {
		r, err = c.fail(ctx, a, "create-unknown")
		if err != nil {
			return r, err
		}
		return r, ErrMissingPod
	}
	workCtx := ctx
	if stopped(r) || r.PodTerminal {
		var cancel context.CancelFunc
		workCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
	}
	raw, exists, err := c.client.Get(workCtx, r.PodNamespace, r.PodName)
	if err != nil {
		return r, err
	}
	if !exists {
		if !stopped(r) && !r.PodTerminal {
			r, err = c.fail(ctx, a, "pod-lost")
			if err != nil {
				return r, err
			}
		}
		if r.PodUID != "" {
			if r.GuestStopped() {
				return c.catalog.ConfirmSandboxDeleted(workCtx, a, r.PodUID)
			}
			unknown, markErr := c.catalog.MarkSandboxCleanupUnknown(workCtx, a)
			if markErr != nil {
				return r, markErr
			}
			return unknown, ErrGuestStopUnknown
		}
		r, err = c.fail(ctx, a, "create-unknown")
		if err != nil {
			return r, err
		}
		return r, ErrMissingPod
	}
	pod, caps, inspectErr := inspectPod(r, raw)
	if inspectErr == nil {
		inspectErr = c.catalog.MatchSandboxCapabilities(workCtx, a, caps)
	}
	if inspectErr != nil {
		failed, err := c.fail(ctx, a, "pod-mismatch")
		if err != nil {
			return r, err
		}
		// A known UID may be cleaned even after its template drifted. An
		// unbound or replacement UID is never adopted or deleted on labels.
		if r.PodUID != "" && pod.uid == r.PodUID {
			if failed, err = c.cleanup(ctx, failed); err != nil {
				return failed, err
			}
		}
		return failed, ErrPod
	}
	if r.PodUID == "" {
		r, err = c.catalog.BindSandboxPod(workCtx, a, pod.uid)
		if err != nil {
			return r, err
		}
	}
	r, err = c.observeGuest(workCtx, r, pod)
	if err != nil {
		return r, err
	}
	if stopped(r) || r.PodTerminal || pod.deleting {
		if pod.deleting && !r.PodTerminal && !stopped(r) {
			r, err = c.fail(ctx, a, "pod-lost")
			if err != nil {
				return r, err
			}
		}
		return c.cleanup(ctx, r)
	}
	return r, nil
}

func (c *Controller) cleanup(ctx context.Context, r platform.SandboxRun) (platform.SandboxRun, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := c.client.Delete(cleanup, r.PodNamespace, r.PodName, r.PodUID); err != nil {
		return r, err
	}
	raw, exists, err := c.client.Get(cleanup, r.PodNamespace, r.PodName)
	if err != nil {
		return r, err
	}
	if exists {
		pod, caps, err := inspectPod(r, raw)
		if err != nil || pod.uid != r.PodUID {
			unknown, e := c.catalog.MarkSandboxCleanupUnknown(cleanup, r.Address())
			if e != nil {
				return r, e
			}
			return unknown, ErrPod
		}
		if err = c.catalog.MatchSandboxCapabilities(cleanup, r.Address(), caps); err != nil {
			return r, err
		}
		r, err = c.observeGuest(cleanup, r, pod)
		if err != nil {
			return r, err
		}
		if !r.GuestStopped() && !pod.neverScheduled {
			return r, nil
		}
		if pod.ownFinalizer {
			if err = c.client.releaseGuestFinalizer(cleanup, r.PodNamespace, r.PodName, pod); err != nil {
				return r, err
			}
			if !r.GuestStopped() {
				// Only the successful exact spec/UID/RV finalizer CAS proves this
				// deleting Pod could not race a scheduler assignment.
				r, err = c.catalog.RecordSandboxGuestStopped(cleanup, r.Address(), platform.SandboxGuestStopProof{Kind: platform.SandboxGuestNeverScheduled, PodUID: pod.uid, ResourceVersion: pod.resourceVersion})
				if err != nil {
					return r, err
				}
			}
		}
		_, stillExists, e := c.client.Get(cleanup, r.PodNamespace, r.PodName)
		if e != nil {
			return r, e
		}
		if stillExists {
			return r, nil
		}
	}
	if !r.GuestStopped() {
		unknown, e := c.catalog.MarkSandboxCleanupUnknown(cleanup, r.Address())
		if e != nil {
			return r, e
		}
		return unknown, ErrGuestStopUnknown
	}
	return c.catalog.ConfirmSandboxDeleted(cleanup, r.Address(), r.PodUID)
}

func (c *Controller) observeGuest(ctx context.Context, r platform.SandboxRun, p observedPod) (platform.SandboxRun, error) {
	var err error
	if p.nodeName != "" {
		previous := r
		r, err = c.catalog.BindSandboxGuestRuntime(ctx, r.Address(), p.uid, p.nodeName, p.containerID)
		if err != nil {
			if errors.Is(err, platform.ErrConflict) {
				failed, e := c.fail(ctx, previous.Address(), "guest-runtime-changed")
				if e != nil {
					return previous, e
				}
				unknown, e := c.catalog.MarkSandboxCleanupUnknown(ctx, previous.Address())
				if e != nil {
					return failed, e
				}
				return unknown, ErrGuestStopUnknown
			}
			return previous, err
		}
	}
	if p.terminal && !r.PodTerminal {
		r, err = c.catalog.RecordSandboxTerminal(ctx, r.Address(), p.uid, p.exit)
		if err != nil {
			return r, err
		}
	} else if p.failed && !stopped(r) {
		r, err = c.fail(ctx, r.Address(), "pod-failed")
		if err != nil {
			return r, err
		}
	}
	if p.stopProof != nil {
		r, err = c.catalog.RecordSandboxGuestStopped(ctx, r.Address(), *p.stopProof)
		if err != nil {
			return r, err
		}
	}
	return r, nil
}

// ReconcileAll only observes persisted fences. A prepared but unstarted run is
// never launched by recovery. Callers can repeat after transient API failures.
func (c *Controller) ReconcileAll(ctx context.Context) error {
	var failures []error
	for scope := range c.profiles {
		runs, err := c.catalog.ListSandboxRuns(ctx, scope)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		for _, r := range runs {
			if r.CreateStarted && !r.PodDeleted {
				if _, err := c.Reconcile(ctx, r.Address()); err != nil {
					failures = append(failures, err)
					if ctx.Err() != nil {
						return errors.Join(failures...)
					}
				}
			}
		}
	}
	return errors.Join(failures...)
}
