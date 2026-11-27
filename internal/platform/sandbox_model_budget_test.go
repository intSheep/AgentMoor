package platform

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intSheep/AgentMoor/internal/asset"
)

func sandboxModelTestPolicy() SandboxModelPolicy {
	return SandboxModelPolicy{Fingerprint: sandboxTestBlob("fixed https upstream/model/request-and-byte-limits, no key value").SHA256,
		MaxRequests: 8, MaxInputBytes: 1024, MaxOutputBytes: 512, PerRequestOutputBytes: 64}
}

func TestSandboxModelBudget(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	checkSandboxModelBudget(t, c, scope("model-budget"))
}

// Run the same reservations against SQLite and actual PostgreSQL/S3.
func checkSandboxModelBudget(t *testing.T, c *Catalog, base asset.Scope) {
	t.Helper()
	allocate := func(t *testing.T, suffix string) SandboxCreatePermit {
		t.Helper()
		s := base
		s.Runtime += "-" + suffix
		return sandboxTestFence(t, c, sandboxTestClaim(t, c, s), "sealed input")
	}
	t.Run("concurrent_requests_cannot_overdraw", func(t *testing.T) {
		p := allocate(t, "concurrent")
		policy := sandboxModelTestPolicy()
		policy.MaxRequests = 7
		policy.MaxInputBytes = 21
		policy.MaxOutputBytes = 35
		policy.PerRequestOutputBytes = 5
		var allowed, denied atomic.Int32
		var mu sync.Mutex
		sequences := map[int64]bool{}
		var workers sync.WaitGroup
		for range 48 {
			workers.Go(func() {
				r, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 3)
				if errors.Is(err, ErrSandboxModelBudget) {
					denied.Add(1)
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				allowed.Add(1)
				if r.Run.ID != p.Run.ID || r.Policy != policy || r.InputBytes != r.Requests*3 || r.OutputBytes != r.Requests*5 {
					t.Error("reservation did not identify its cumulative charge")
				}
				mu.Lock()
				if sequences[r.Requests] {
					t.Error("duplicate reservation sequence")
				}
				sequences[r.Requests] = true
				mu.Unlock()
			})
		}
		workers.Wait()
		if allowed.Load() != 7 || denied.Load() != 41 {
			t.Fatalf("overdrawn shared capability: allowed=%d denied=%d", allowed.Load(), denied.Load())
		}
		r, err := c.readSandboxModelBudget(t.Context(), p.Run)
		if err != nil || r.Requests != 7 || r.InputBytes != 21 || r.OutputBytes != 35 {
			t.Fatal("wrong durable total", r, err)
		}
	})
	t.Run("policy_drift_never_changes_existing_allowance", func(t *testing.T) {
		p := allocate(t, "policy")
		policy := sandboxModelTestPolicy()
		if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 3); err != nil {
			t.Fatal(err)
		}
		for name, change := range map[string]func(*SandboxModelPolicy){
			"upstream-fingerprint": func(p *SandboxModelPolicy) {
				p.Fingerprint = sandboxTestBlob("different upstream host or model").SHA256
			},
			"requests": func(p *SandboxModelPolicy) { p.MaxRequests++ }, "input": func(p *SandboxModelPolicy) { p.MaxInputBytes++ },
			"output": func(p *SandboxModelPolicy) { p.MaxOutputBytes++ }, "per-response": func(p *SandboxModelPolicy) { p.PerRequestOutputBytes++ },
		} {
			t.Run(name, func(t *testing.T) {
				changed := policy
				change(&changed)
				if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, changed, 3); !errors.Is(err, ErrConflict) {
					t.Fatal("changed frozen policy admitted", err)
				}
			})
		}
		r, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 4)
		if err != nil || r.Requests != 2 || r.InputBytes != 7 || r.OutputBytes != 128 {
			t.Fatal("rejected drift changed original ledger", r, err)
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{p.Capabilities.Input, p.Capabilities.Output, p.Capabilities.Model, p.Run.Attempt} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("reservation serialized private authority")
			}
		}
		var stored string
		if err := c.db.QueryRowContext(t.Context(), "SELECT policy FROM sandbox_model_budgets WHERE scope=$1 AND run_id=$2", p.Run.Scope.Prefix(), p.Run.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stored, p.Capabilities.Model) {
			t.Fatal("raw capability persisted in model policy")
		}
	})

	t.Run("exact_limits_and_one_over", func(t *testing.T) {
		for _, which := range []string{"requests", "input", "output"} {
			t.Run(which, func(t *testing.T) {
				p := allocate(t, "limit-"+which)
				policy := sandboxModelTestPolicy()
				input := int64(3)
				switch which {
				case "requests":
					policy.MaxRequests = 2
				case "input":
					policy.MaxInputBytes = 6
				case "output":
					policy.MaxOutputBytes = 128
				}
				for want := int64(1); want <= 2; want++ {
					r, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, input)
					if err != nil || r.Requests != want {
						t.Fatal("exact allowance rejected", r, err)
					}
				}
				if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); !errors.Is(err, ErrSandboxModelBudget) {
					t.Fatal("one over allowance admitted", err)
				}
			})
		}
		for _, which := range []string{"hard-request-count", "hard-total-input", "hard-total-output", "hard-input-per-request", "hard-output-per-request"} {
			t.Run(which, func(t *testing.T) {
				p := allocate(t, "hard-"+which)
				policy := SandboxModelPolicy{Fingerprint: sandboxModelTestPolicy().Fingerprint, MaxRequests: SandboxModelMaxRequests, MaxInputBytes: SandboxModelMaxInputBytes, MaxOutputBytes: SandboxModelMaxOutputBytes, PerRequestOutputBytes: 1}
				calls, input := int64(1), int64(1)
				switch which {
				case "hard-request-count":
					calls = SandboxModelMaxRequests
				case "hard-total-input":
					calls = SandboxModelMaxInputBytes / SandboxModelMaxRequestInput
					input = SandboxModelMaxRequestInput
				case "hard-total-output":
					policy.PerRequestOutputBytes = SandboxModelMaxRequestOutput
					calls = SandboxModelMaxOutputBytes / SandboxModelMaxRequestOutput
				case "hard-input-per-request":
					input = SandboxModelMaxRequestInput
				case "hard-output-per-request":
					policy.PerRequestOutputBytes = SandboxModelMaxRequestOutput
				}
				for range calls {
					if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, input); err != nil {
						t.Fatal("hard exact limit rejected", err)
					}
				}
				if strings.Contains(which, "total") || which == "hard-request-count" {
					if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); !errors.Is(err, ErrSandboxModelBudget) {
						t.Fatal("hard total allowance exceeded", err)
					}
				}
			})
		}
	})

	t.Run("invalid_bounds_do_not_create_a_ledger", func(t *testing.T) {
		p := allocate(t, "invalid")
		base := sandboxModelTestPolicy()
		for name, change := range map[string]func(*SandboxModelPolicy){
			"missing-fingerprint": func(p *SandboxModelPolicy) { p.Fingerprint = "" }, "uppercase-fingerprint": func(p *SandboxModelPolicy) { p.Fingerprint = strings.ToUpper(p.Fingerprint) },
			"zero-requests": func(p *SandboxModelPolicy) { p.MaxRequests = 0 }, "negative-requests": func(p *SandboxModelPolicy) { p.MaxRequests = -1 }, "huge-requests": func(p *SandboxModelPolicy) { p.MaxRequests = math.MaxInt64 },
			"one-over-requests": func(p *SandboxModelPolicy) { p.MaxRequests = SandboxModelMaxRequests + 1 },
			"zero-input":        func(p *SandboxModelPolicy) { p.MaxInputBytes = 0 },
			"negative-input":    func(p *SandboxModelPolicy) { p.MaxInputBytes = -1 }, "huge-input": func(p *SandboxModelPolicy) { p.MaxInputBytes = SandboxModelMaxInputBytes + 1 },
			"zero-output":     func(p *SandboxModelPolicy) { p.MaxOutputBytes = 0 },
			"negative-output": func(p *SandboxModelPolicy) { p.MaxOutputBytes = -1 },
			"one-over-output": func(p *SandboxModelPolicy) { p.MaxOutputBytes = SandboxModelMaxOutputBytes + 1 }, "huge-output": func(p *SandboxModelPolicy) { p.MaxOutputBytes = math.MaxInt64 },
			"zero-per-response":     func(p *SandboxModelPolicy) { p.PerRequestOutputBytes = 0 },
			"negative-per-response": func(p *SandboxModelPolicy) { p.PerRequestOutputBytes = -1 }, "huge-per-response": func(p *SandboxModelPolicy) { p.PerRequestOutputBytes = SandboxModelMaxRequestOutput + 1 },
			"response-exceeds-total": func(p *SandboxModelPolicy) { p.PerRequestOutputBytes = p.MaxOutputBytes + 1 },
		} {
			t.Run(name, func(t *testing.T) {
				policy := base
				change(&policy)
				if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); !errors.Is(err, errBadRequest) {
					t.Fatal("invalid policy admitted", err)
				}
			})
		}
		for _, n := range []int64{-1, 0, SandboxModelMaxRequestInput + 1, math.MaxInt64, math.MinInt64} {
			if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, base, n); !errors.Is(err, errBadRequest) {
				t.Fatal("invalid input size admitted", n, err)
			}
		}
		if _, err := c.readSandboxModelBudget(t.Context(), p.Run); !errors.Is(err, ErrMissing) {
			t.Fatal("invalid calls created ledger", err)
		}
	})

	t.Run("only_active_current_model_authority", func(t *testing.T) {
		for _, stop := range []string{"wrong-input-purpose", "wrong-output-purpose", "unknown-token", "revoked", "stale-attempt", "cancel-task", "cancel-sandbox", "failed", "finish", "output-sealed", "pod-terminal", "pod-deleted", "expired"} {
			t.Run(stop, func(t *testing.T) {
				p := allocate(t, "auth-"+stop)
				token := p.Capabilities.Model
				policy := sandboxModelTestPolicy()
				if _, err := c.ReserveSandboxModel(t.Context(), token, policy, 1); err != nil {
					t.Fatal("authority positive control", err)
				}
				switch stop {
				case "wrong-input-purpose":
					token = p.Capabilities.Input
				case "wrong-output-purpose":
					token = p.Capabilities.Output
				case "unknown-token":
					token = "sbx1." + strings.Repeat("A", 43)
				case "revoked":
					if _, err := c.writer.ExecContext(t.Context(), "UPDATE sandbox_caps SET revoked=1 WHERE token_hash=$1", sandboxTokenHash(token)); err != nil {
						t.Fatal(err)
					}
				case "stale-attempt":
					if _, err := c.changeTask(t.Context(), taskAddress{Scope: p.Run.Scope}, false, func(slot *taskSlot) error { slot.attempt = uuid.NewString(); return nil }); err != nil {
						t.Fatal(err)
					}
				case "cancel-task":
					if _, err := c.cancelTask(t.Context(), taskAddress{Scope: p.Run.Scope}); err != nil {
						t.Fatal(err)
					}
				case "cancel-sandbox":
					if _, err := c.CancelSandbox(t.Context(), p.Run.Address()); err != nil {
						t.Fatal(err)
					}
				case "failed":
					if _, err := c.FailSandbox(t.Context(), p.Run.Address(), "owned-fixture-failure"); err != nil {
						t.Fatal(err)
					}
				case "finish":
					sandboxTestFinish(t, c, p.Run.Address(), false)
				case "output-sealed":
					if _, err := c.SealSandboxOutput(t.Context(), p.Capabilities.Output, sandboxTestBlob("result"), strings.NewReader("result")); err != nil {
						t.Fatal(err)
					}
				case "pod-terminal", "pod-deleted":
					uid := uuid.NewString()
					if _, err := c.BindSandboxPod(t.Context(), p.Run.Address(), uid); err != nil {
						t.Fatal(err)
					}
					if stop == "pod-terminal" {
						if _, err := c.RecordSandboxTerminal(t.Context(), p.Run.Address(), uid, 1); err != nil {
							t.Fatal(err)
						}
					} else {
						// Deletion requires a separate trusted stop observation;
						// no other denial case is given that authority.
						sandboxTestGuestStop(t, c, p.Run.Address(), uid, 1)
						if _, err := c.ConfirmSandboxDeleted(t.Context(), p.Run.Address(), uid); err != nil {
							t.Fatal(err)
						}
					}
				case "expired":
					if _, err := c.writer.ExecContext(t.Context(), "UPDATE sandbox_caps SET expires=$1 WHERE token_hash=$2", time.Now().Add(-time.Second).UnixNano(), sandboxTokenHash(token)); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := c.ReserveSandboxModel(t.Context(), token, policy, 1); !errors.Is(err, ErrSandboxDenied) {
					t.Fatal("stopped/foreign authority consumed model allowance", err)
				}
				r, err := c.readSandboxModelBudget(t.Context(), p.Run)
				if err != nil || r.Requests != 1 || r.InputBytes != 1 {
					t.Fatal("denial changed durable counters", r, err)
				}
			})
		}
	})

	t.Run("original_task_deadline_is_not_renewed", func(t *testing.T) {
		s := base
		s.Runtime += "-original-deadline"
		a := sandboxTestClaim(t, c, s)
		if _, err := c.changeTask(t.Context(), taskAddress{Scope: s}, false, func(slot *taskSlot) error {
			slot.Deadline = time.Now().UTC().Add(500 * time.Millisecond)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		p := sandboxTestFence(t, c, a, "input")
		policy := sandboxModelTestPolicy()
		if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); err != nil {
			t.Fatal("deadline positive control", err)
		}
		time.Sleep(time.Until(p.Run.Deadline) + 10*time.Millisecond)
		if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); !errors.Is(err, ErrSandboxDenied) {
			t.Fatal("reservation renewed task deadline", err)
		}
		r, err := c.readSandboxModelBudget(t.Context(), p.Run)
		if err != nil || r.Requests != 1 {
			t.Fatal("expiry changed charge", r, err)
		}
	})

	t.Run("separate_runs_have_separate_ledgers", func(t *testing.T) {
		policy := sandboxModelTestPolicy()
		policy.MaxRequests = 1
		p, q := allocate(t, "separate-a"), allocate(t, "separate-b")
		for _, permit := range []SandboxCreatePermit{p, q} {
			r, err := c.ReserveSandboxModel(t.Context(), permit.Capabilities.Model, policy, 1)
			if err != nil || r.Requests != 1 || r.Run.ID != permit.Run.ID {
				t.Fatal("run shared another ledger", r, err)
			}
		}
	})

	t.Run("corrupt_counters_fail_closed_before_arithmetic", func(t *testing.T) {
		for _, column := range []string{"requests", "input_bytes", "output_bytes"} {
			t.Run(column, func(t *testing.T) {
				p := allocate(t, "corrupt-"+column)
				policy := sandboxModelTestPolicy()
				if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); err != nil {
					t.Fatal(err)
				}
				if _, err := c.writer.ExecContext(t.Context(), "UPDATE sandbox_model_budgets SET "+column+"=$1 WHERE scope=$2 AND run_id=$3", int64(math.MaxInt64), p.Run.Scope.Prefix(), p.Run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); !errors.Is(err, ErrConflict) {
					t.Fatal("corrupt ledger arithmetic admitted", err)
				}
			})
		}
	})
}

func TestSandboxModelBudgetSurvivesCatalogRestart(t *testing.T) {
	dir, objects := t.TempDir(), DiskObjects{t.TempDir()}
	c, err := OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	verify := prepareSandboxModelRestart(t, c, scope("model-budget-restart"))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCatalog(t.Context(), dir, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	verify(c)
}

func prepareSandboxModelRestart(t *testing.T, c *Catalog, s asset.Scope) func(*Catalog) {
	t.Helper()
	a := sandboxTestClaim(t, c, s)
	p := sandboxTestFence(t, c, a, "frozen request")
	policy := sandboxModelTestPolicy()
	policy.MaxRequests = 2
	// Discard this successful return as if the proxy lost its acknowledgement.
	if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 3); err != nil {
		t.Fatal(err)
	}
	return func(reopened *Catalog) {
		t.Helper()
		r, err := reopened.readSandboxModelBudget(t.Context(), p.Run)
		if err != nil || r.Requests != 1 || r.InputBytes != 3 || r.OutputBytes != 64 {
			t.Fatal("restart reset forgotten charge", r, err)
		}
		r, err = reopened.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 5)
		if err != nil || r.Requests != 2 || r.InputBytes != 8 || r.OutputBytes != 128 {
			t.Fatal("restart did not preserve cumulative reservation", r, err)
		}
		if _, err := reopened.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 1); !errors.Is(err, ErrSandboxModelBudget) {
			t.Fatal("restart granted extra allowance", err)
		}
	}
}

func prepareSandboxModelLeaseFailure(t *testing.T, c *Catalog) func() {
	t.Helper()
	p := sandboxTestFence(t, c, sandboxTestClaim(t, c, scope("model-budget-lease-existing")), "input")
	q := sandboxTestFence(t, c, sandboxTestClaim(t, c, scope("model-budget-lease-new")), "input")
	policy := sandboxModelTestPolicy()
	if _, err := c.ReserveSandboxModel(t.Context(), p.Capabilities.Model, policy, 3); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		for _, permit := range []SandboxCreatePermit{p, q} {
			if _, err := c.ReserveSandboxModel(t.Context(), permit.Capabilities.Model, policy, 3); err == nil {
				t.Fatal("lost writer lease granted model reservation")
			}
		}
		r, err := c.readSandboxModelBudget(t.Context(), p.Run)
		if err != nil || r.Requests != 1 || r.InputBytes != 3 {
			t.Fatal("lost lease changed existing charge", r, err)
		}
		if _, err := c.readSandboxModelBudget(t.Context(), q.Run); !errors.Is(err, ErrMissing) {
			t.Fatal("lost lease created fresh ledger", err)
		}
	}
}

func TestSandboxModelBudgetCancelledContextDoesNotCharge(t *testing.T) {
	c, err := OpenCatalog(t.Context(), t.TempDir(), "", DiskObjects{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := sandboxTestFence(t, c, sandboxTestClaim(t, c, scope("model-budget-context")), "input")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.ReserveSandboxModel(ctx, p.Capabilities.Model, sandboxModelTestPolicy(), 1); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled call granted reservation", err)
	}
	if _, err := c.readSandboxModelBudget(t.Context(), p.Run); !errors.Is(err, ErrMissing) {
		t.Fatal("cancelled call created ledger", err)
	}
}
