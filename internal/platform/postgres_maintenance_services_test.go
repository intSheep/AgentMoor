//go:build integration

package platform

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentmoor/internal/task"
)

// This PostgreSQL v3 plaintext proxy never logs or persists protocol payloads.
// BackendKeyData contributes only its PID; the adjacent cancellation secret
// and authentication messages are forwarded unchanged and never retained.
type postgresMaintenanceProxy struct {
	listener net.Listener
	upstream string
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	pairs    map[*postgresMaintenancePair]bool
	wg       sync.WaitGroup
	close    sync.Once
	target   atomic.Int64
	armed    atomic.Bool
	scanArm  atomic.Bool
	scanPID  atomic.Int64
	lastScan atomic.Int64
	scanRows atomic.Int64
	scanACKs atomic.Int64
	matched  atomic.Int64
	acks     atomic.Int64
	commits  atomic.Int64
	invalid  atomic.Int64
	blocked  chan struct{}
	block    sync.Once
}

type postgresMaintenancePair struct {
	proxy                     *postgresMaintenanceProxy
	front                     net.Conn
	mu                        sync.Mutex
	back                      net.Conn
	dead                      bool
	pid                       atomic.Int64
	drop                      atomic.Bool
	scanDrop                  atomic.Bool
	statement, portal         string
	scanStatement, scanPortal bool
}

func newPostgresMaintenanceProxy(t *testing.T, dsn string) (*postgresMaintenanceProxy, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() == "" || u.Port() == "" || u.Query().Get("sslmode") != "disable" {
		t.Fatal("owned PostgreSQL wire fixture requires an explicit plaintext URL DSN")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot bind owned PostgreSQL fault proxy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &postgresMaintenanceProxy{listener: listener, upstream: net.JoinHostPort(u.Hostname(), u.Port()), ctx: ctx, cancel: cancel,
		pairs: make(map[*postgresMaintenancePair]bool), blocked: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			front, err := p.listener.Accept()
			if err != nil {
				return
			}
			pair := &postgresMaintenancePair{proxy: p, front: front}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				front.Close()
				return
			}
			p.pairs[pair] = true
			p.wg.Add(1)
			p.mu.Unlock()
			go pair.serve()
		}
	}()
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	u.Host = listener.Addr().String()
	return p, u.String()
}

func (p *postgresMaintenanceProxy) Close() error {
	p.close.Do(func() {
		p.cancel()
		p.mu.Lock()
		p.closed = true
		p.listener.Close()
		for pair := range p.pairs {
			pair.shutdown()
		}
		p.mu.Unlock()
	})
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("owned PostgreSQL fault proxy did not join within its cleanup bound")
	}
}

func (p *postgresMaintenanceProxy) arm(t *testing.T, pid int) {
	t.Helper()
	observed := false
	p.mu.Lock()
	for pair := range p.pairs {
		observed = observed || pair.pid.Load() == int64(pid)
	}
	p.mu.Unlock()
	if !observed {
		t.Fatal("leased writer PID was not observed on its actual proxy connection")
	}
	p.target.Store(int64(pid))
	p.armed.Store(true)
}

func (p *postgresMaintenancePair) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	p.front.Close()
	if p.back != nil {
		p.back.Close()
	}
}

func (p *postgresMaintenancePair) serve() {
	defer p.proxy.wg.Done()
	defer func() {
		p.shutdown()
		p.proxy.mu.Lock()
		delete(p.proxy.pairs, p)
		p.proxy.mu.Unlock()
	}()
	back, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(p.proxy.ctx, "tcp", p.proxy.upstream)
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		back.Close()
		return
	}
	p.back = back
	p.mu.Unlock()
	var size [4]byte
	if _, err := io.ReadFull(p.front, size[:]); err != nil {
		return
	}
	length := binary.BigEndian.Uint32(size[:])
	if length < 8 || length > 64<<10 {
		p.proxy.invalid.Add(1)
		return
	}
	startup := make([]byte, int(length))
	copy(startup, size[:])
	if _, err := io.ReadFull(p.front, startup[4:]); err != nil {
		return
	}
	code := binary.BigEndian.Uint32(startup[4:8])
	if code != 196608 && code != 80877102 { // protocol v3 or cancellation packet
		p.proxy.invalid.Add(1)
		return
	}
	if err := postgresMaintenanceWrite(back, startup); err != nil {
		return
	}
	if code == 80877102 {
		return // A real cancellation request has no backend response.
	}
	done := make(chan struct{}, 2)
	go func() { p.frontend(); done <- struct{}{} }()
	go func() { p.backend(); done <- struct{}{} }()
	<-done
	p.shutdown()
	<-done
}

func postgresMaintenanceRead(r io.Reader) ([]byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length < 4 || length > 16<<20 {
		return nil, errors.New("owned PostgreSQL frame exceeds the fixture bound")
	}
	frame := make([]byte, int(length)+1)
	copy(frame, header[:])
	_, err := io.ReadFull(r, frame[5:])
	return frame, err
}

func postgresMaintenanceWrite(w io.Writer, frame []byte) error {
	for len(frame) != 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

func postgresMaintenanceNames(frame []byte) (string, string, bool) {
	if len(frame) > 4096 {
		return "", "", false
	}
	body := frame[5:]
	first := bytes.IndexByte(body, 0)
	if first < 0 {
		return "", "", false
	}
	second := bytes.IndexByte(body[first+1:], 0)
	if second < 0 {
		return "", "", false
	}
	return string(body[:first]), string(body[first+1 : first+1+second]), true
}

func (p *postgresMaintenancePair) frontend() {
	for {
		frame, err := postgresMaintenanceRead(p.front)
		if err != nil {
			return
		}
		// Parameterized keysets use Parse/Describe before Bind/Execute. Keep
		// that preparation intact; begin withholding only at the real Execute.
		if frame[0] == 'P' {
			name, query, ok := postgresMaintenanceNames(frame)
			p.statement, p.scanPortal = name, false
			p.scanStatement = ok && (strings.HasSuffix(query, " FROM tasks WHERE scope>$1 ORDER BY scope LIMIT $2") || strings.HasSuffix(query, " FROM turns WHERE (scope,id)>($1,$2) ORDER BY scope,id LIMIT $3"))
		} else if frame[0] == 'B' {
			portal, statement, ok := postgresMaintenanceNames(frame)
			p.portal, p.scanPortal = portal, ok && p.scanStatement && statement == p.statement
		} else if frame[0] == 'E' && p.scanPortal {
			end := bytes.IndexByte(frame[5:], 0)
			if end >= 0 && string(frame[5:5+end]) == p.portal && p.pid.Load() != p.proxy.target.Load() {
				p.proxy.lastScan.Store(p.pid.Load())
				if p.proxy.scanArm.CompareAndSwap(true, false) {
					p.proxy.scanPID.Store(p.pid.Load())
					p.scanDrop.Store(true)
				}
			}
		}
		// lib/pq uses a simple Query for BeginTx. Inspect only this bounded
		// command; authentication, SQL parameters and results stay opaque.
		if frame[0] == 'Q' && len(frame) <= 128 && frame[len(frame)-1] == 0 && p.pid.Load() == p.proxy.target.Load() {
			command := strings.TrimSpace(string(frame[5 : len(frame)-1]))
			if (strings.EqualFold(command, "BEGIN") || strings.EqualFold(command, "BEGIN READ WRITE")) && p.proxy.armed.CompareAndSwap(true, false) {
				p.drop.Store(true)
				p.proxy.matched.Add(1)
			}
			if strings.EqualFold(command, "COMMIT") {
				p.proxy.commits.Add(1)
			}
		}
		if err := postgresMaintenanceWrite(p.back, frame); err != nil {
			return
		}
	}
}

func (p *postgresMaintenancePair) backend() {
	beginACK := false
	selectACK := false
	for {
		frame, err := postgresMaintenanceRead(p.back)
		if err != nil {
			return
		}
		if frame[0] == 'K' && len(frame) == 13 {
			p.pid.Store(int64(binary.BigEndian.Uint32(frame[5:9])))
		}
		if p.scanDrop.Load() {
			if frame[0] == 'D' {
				p.proxy.scanRows.Add(1)
			} else if frame[0] == 'C' && bytes.HasPrefix(frame[5:], []byte("SELECT ")) {
				selectACK = true
			} else if frame[0] == 'Z' && selectACK && len(frame) == 6 && frame[5] == 'I' {
				p.proxy.scanACKs.Add(1)
				p.proxy.block.Do(func() { close(p.proxy.blocked) })
			}
			continue
		}
		if p.drop.Load() {
			if frame[0] == 'C' && bytes.Equal(frame[5:], []byte("BEGIN\x00")) {
				beginACK = true
				continue // Swallow the actual backend CommandComplete.
			}
			if frame[0] == 'Z' {
				p.drop.Store(false)
				if beginACK && len(frame) == 6 && frame[5] == 'T' {
					p.proxy.acks.Add(1)
					p.proxy.block.Do(func() { close(p.proxy.blocked) })
					continue // Swallow only BEGIN's ReadyForQuery too.
				}
			}
		}
		if err := postgresMaintenanceWrite(p.front, frame); err != nil {
			return
		}
	}
}

func checkPostgresMaintenanceBlackholes(t *testing.T, objects Objects, dsn string) {
	t.Helper()
	checkPostgresMaintenanceBlackhole(t, objects, dsn, false, false)
	checkPostgresMaintenanceBlackhole(t, objects, dsn, true, false)
	checkPostgresMaintenanceBlackhole(t, objects, dsn, false, true)
	checkPostgresMaintenanceBlackhole(t, objects, dsn, true, true)
}

func checkPostgresMaintenanceBlackhole(t *testing.T, objects Objects, dsn string, controller, scan bool) {
	t.Helper()
	direct, reader, isolatedDSN := openOwnedAuditLinksPostgres(t, objects, dsn)
	scenario := "sweep"
	if controller {
		scenario = "controller_cancel"
	}
	if scan {
		scenario = "scan_" + scenario
	}
	row := deadlineSweepFixtureRow(t, scope("deadline-begin-blackhole-"+scenario), "", task.Running, time.Now().UTC().Add(-time.Hour))
	deadlineSweepInsert(t, direct, row)
	if err := direct.Close(); err != nil {
		t.Fatal(err)
	}
	proxy, proxyDSN := newPostgresMaintenanceProxy(t, isolatedDSN)
	setup, stopSetup := context.WithTimeout(t.Context(), 5*time.Second)
	c, err := OpenCatalog(setup, t.TempDir(), proxyDSN, objects)
	stopSetup()
	if err != nil {
		t.Fatal("cannot open owned PostgreSQL Catalog through the plaintext fixture")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan deadlineSweepPGOutcome, 1)
	started, received, ownerClosed := false, false, false
	closeOwner := func() {
		if ownerClosed {
			return
		}
		ownerClosed = true
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
		closed := make(chan error, 1)
		go func() { closed <- c.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				t.Error("cannot close owned failed PostgreSQL Catalog")
			}
		case <-time.After(5 * time.Second):
			t.Error("failed PostgreSQL Catalog did not close within its cleanup bound")
		}
	}
	t.Cleanup(func() {
		cancel()
		// Closing only the owned sockets also releases an unexpected stuck
		// baseline, so a failing regression cannot wedge the service harness.
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
		if started && !received {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("blackholed maintenance did not join during bounded cleanup")
			}
		}
		closeOwner()
	})
	pid := deadlineSweepPGPID(t, c)
	proxy.arm(t, pid)
	if scan {
		proxy.armed.Store(false)
		proxy.scanArm.Store(true)
	}
	start := time.Now()
	started = true
	var reports atomic.Int64
	go func() {
		if controller {
			c.RunDeadlineController(ctx, func() { reports.Add(1) })
			done <- deadlineSweepPGOutcome{}
			return
		}
		result, err := c.SweepExpired(ctx, DeadlineSweepCursor{})
		done <- deadlineSweepPGOutcome{result, err}
	}()
	select {
	case <-proxy.blocked:
	case outcome := <-done:
		received = true
		t.Fatal("maintenance returned before the selected actual PostgreSQL response was swallowed", outcome.result, outcome.err)
	case <-time.After(3 * time.Second):
		t.Fatal("target PostgreSQL session never reached its selected real response blackhole")
	}
	observe, stopObserve := context.WithTimeout(t.Context(), time.Second)
	var realTransaction bool
	observePID, expectedState := int64(pid), "idle in transaction"
	if scan {
		observePID, expectedState = proxy.scanPID.Load(), "idle"
	}
	observeErr := reader.db.QueryRowContext(observe, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND state=$2)", observePID, expectedState).Scan(&realTransaction)
	var one int
	poolErr := c.db.QueryRowContext(observe, "SELECT 1").Scan(&one)
	stopObserve()
	if observeErr != nil || !realTransaction || poolErr != nil || one != 1 || (!scan && (proxy.matched.Load() != 1 || proxy.acks.Load() != 1)) || (scan && (observePID == int64(pid) || proxy.scanRows.Load() < 1 || proxy.scanACKs.Load() != 1)) {
		t.Fatal("fixture did not isolate the selected completed SQL response while retaining pooled reads")
	}
	joinStart := start
	if controller {
		joinStart = time.Now()
		cancel()
	}
	var outcome deadlineSweepPGOutcome
	bound := deadlineSweepWriteIO
	if scan {
		bound = deadlineSweepBudget
	}
	select {
	case outcome = <-done:
		received = true
	case <-time.After(bound + 2*time.Second):
		t.Fatal("response blackhole exceeded the scoped maintenance I/O/join bound")
	}
	elapsed := time.Since(joinStart)
	if elapsed > bound+2*time.Second || (!controller && (outcome.err == nil || outcome.result.Interrupted != 0 || outcome.result.Cursor != (DeadlineSweepCursor{}))) {
		t.Fatal("blackholed SQL response reported success or ignored its maintenance bound", outcome.result, elapsed)
	}
	deadlineSweepRequireStored(t, c, row, false)
	if len(auditEvents(t, reader, row.address.Scope)) != 0 || proxy.commits.Load() != 0 || proxy.invalid.Load() != 0 {
		t.Fatal("blackholed BEGIN committed, audited or violated the fixture protocol bounds")
	}
	if scan {
		deadlineSweepPGHealthy(t, c, pid, scenario)
		result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{})
		if err != nil || result.Interrupted != 1 {
			t.Fatal("fresh reader did not recover after the precise read-response blackhole", result, err)
		}
		events := auditEvents(t, reader, row.address.Scope)
		if len(events) != 1 {
			t.Fatal("read-response recovery duplicated expiry audit")
		}
		deadlineSweepRequireEvent(t, events[0], row)
		deadlineSweepRequireStored(t, c, row, true)
		if result, err := c.SweepExpired(t.Context(), DeadlineSweepCursor{}); err != nil || result.Interrupted != 0 || !reflect.DeepEqual(events, auditEvents(t, reader, row.address.Scope)) {
			t.Fatal("healthy repeated scan wrote another transition", result, err)
		}
		reuseCtx, stopReuse := context.WithTimeout(t.Context(), 3*time.Second)
		reused, err := c.db.Conn(reuseCtx)
		if err != nil {
			stopReuse()
			t.Fatal("cannot borrow the completed scan reader")
		}
		var reusedPID int64
		var ignored any
		pidErr := reused.QueryRowContext(reuseCtx, "SELECT pg_backend_pid()").Scan(&reusedPID)
		sleepErr := reused.QueryRowContext(reuseCtx, "SELECT pg_sleep(2.1)").Scan(&ignored)
		reused.Close()
		stopReuse()
		if pidErr != nil || reusedPID != proxy.lastScan.Load() || sleepErr != nil {
			t.Fatal("same physical scan reader retained its old 2s socket deadline", pidErr, sleepErr)
		}
		t.Logf("POSTGRES_MAINTENANCE_BLACKHOLE scenario=%s actual_select_completed=true discarded_data_rows=%d discarded_ready=%d elapsed_ms=%d same_writer_healthy=true recovered_once=true replay_interrupted=0 same_reader_reused=true reader_sleep_ms=2100", scenario, proxy.scanRows.Load(), proxy.scanACKs.Load(), elapsed.Milliseconds())
		closeOwner()
		return
	}
	failClosed, stopFailClosed := context.WithTimeout(t.Context(), time.Second)
	var replacementPID int
	writerErr := c.writer.QueryRowContext(failClosed, "SELECT pg_backend_pid()").Scan(&replacementPID)
	_, submitErr := c.submitTask(failClosed, taskAddress{Scope: scope("deadline-blackhole-must-not-fallback")}, task.Spec{Instruction: "must not use a pooled replacement writer", Output: "reply.txt"})
	stopFailClosed()
	t.Logf("POSTGRES_MAINTENANCE_BLACKHOLE scenario=%s begin_forwarded=%d backend_begin_ack_dropped=%d actual_transaction=true pooled_reads_ok=true elapsed_ms=%d commit_queries=%d writer_failed_closed=%t submit_failed_closed=%t controller_reports=%d", scenario, proxy.matched.Load(), proxy.acks.Load(), elapsed.Milliseconds(), proxy.commits.Load(), writerErr != nil, submitErr != nil, reports.Load())
	if writerErr == nil || submitErr == nil {
		t.Fatal("unexpected PostgreSQL I/O failure silently used the pooled reader as a replacement writer")
	}
	closeOwner()
	replacement, err := OpenCatalog(t.Context(), t.TempDir(), isolatedDSN, objects)
	if err != nil {
		t.Fatal("new direct owner could not recover after closing the blackholed writer")
	}
	defer replacement.Close()
	result, err := replacement.SweepExpired(t.Context(), DeadlineSweepCursor{})
	if err != nil || result.Interrupted != 1 {
		t.Fatal("new healthy owner could not recover the original frozen deadline", result, err)
	}
	deadlineSweepRequireStored(t, replacement, row, true)
	events := auditEvents(t, reader, row.address.Scope)
	if len(events) != 1 {
		t.Fatal("new owner did not record one recovered expiry", len(events))
	}
	deadlineSweepRequireEvent(t, events[0], row)
	t.Logf("POSTGRES_MAINTENANCE_BLACKHOLE scenario=%s replacement_interrupted=1 original_bytes_preserved=true recovered_event_once=true", scenario)
}
