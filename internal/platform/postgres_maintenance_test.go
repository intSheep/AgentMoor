package platform

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
)

type maintenanceDeadlineRecorder struct {
	net.Conn
	mu          sync.Mutex
	read, write time.Time
}

func (c *maintenanceDeadlineRecorder) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	c.read = d
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(d)
}
func (c *maintenanceDeadlineRecorder) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	c.write = d
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(d)
}
func (c *maintenanceDeadlineRecorder) deadlines() (time.Time, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read, c.write
}

func TestPostgresMaintenanceClampsAndRestoresIndependentDriverDeadlines(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	recorded := &maintenanceDeadlineRecorder{Conn: a}
	owner := &postgresSockets{connections: make(map[*postgresSocket]struct{})}
	wrapped := &postgresSocket{Conn: recorded, owner: owner}
	owner.connections[wrapped] = struct{}{}
	now := time.Now()
	window, read, write := now.Add(time.Minute), now.Add(2*time.Minute), now.Add(30*time.Second)
	if err := wrapped.SetReadDeadline(read); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.SetWriteDeadline(write); err != nil {
		t.Fatal(err)
	}
	if err := owner.setMaintenance(window); err != nil {
		t.Fatal(err)
	}
	gotRead, gotWrite := recorded.deadlines()
	if !gotRead.Equal(window) || !gotWrite.Equal(write) {
		t.Fatal("maintenance widened the driver's shorter deadline or missed its read bound")
	}
	// pq may clear its connection/startup timeout while the maintenance window
	// is active. That must not clear either direction of the owned I/O fuse.
	if err := wrapped.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	gotRead, gotWrite = recorded.deadlines()
	if !gotRead.Equal(window) || !gotWrite.Equal(window) {
		t.Fatal("driver clearing its own deadline escaped the maintenance window")
	}
	if err := wrapped.SetReadDeadline(read); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.SetWriteDeadline(write); err != nil {
		t.Fatal(err)
	}
	if err := owner.setMaintenance(time.Time{}); err != nil {
		t.Fatal("ordinary maintenance completion could not clear its window", err)
	}
	gotRead, gotWrite = recorded.deadlines()
	if !gotRead.Equal(read) || !gotWrite.Equal(write) {
		t.Fatal("clear lost the driver's independent original read/write deadlines")
	}
	if err := wrapped.Close(); err != nil {
		t.Fatal(err)
	}
	if len(owner.connections) != 0 {
		t.Fatal("closed socket stayed in the maintenance owner")
	}
}

func TestPostgresMaintenanceRealReadWriteFuseCannotBeClearedAfterExpiry(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	owner := &postgresSockets{connections: make(map[*postgresSocket]struct{})}
	wrapped := &postgresSocket{Conn: a, owner: owner}
	owner.connections[wrapped] = struct{}{}
	window := time.Now().Add(200 * time.Millisecond)
	if err := owner.setMaintenance(window); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { _, err := wrapped.Read(make([]byte, 1)); results <- err }()
	go func() { _, err := wrapped.Write([]byte{1}); results <- err }()
	for range 2 {
		select {
		case err := <-results:
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatal("owned transport did not fail with its actual I/O deadline", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("driver clearing its deadline left a blocked transport unbounded")
		}
	}
	if err := owner.setMaintenance(time.Time{}); !errors.Is(err, driver.ErrBadConn) || !owner.deadline.Equal(window) {
		t.Fatal("expired transport fuse was cleared into a reusable connection", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// pq can start its auxiliary cancellation dial after the main I/O failed.
	// Its fresh background timeout must not outlive the retained expired fuse.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	conn, err := owner.DialContext(ctx, "tcp", listener.Addr().String())
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("late cancellation dial escaped the expired maintenance window", err)
	}
}

func TestPostgresMaintenanceNewAuxiliarySocketInheritsWindowOnlyOfItsOwner(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	owner := &postgresSockets{connections: make(map[*postgresSocket]struct{})}
	other := &postgresSockets{connections: make(map[*postgresSocket]struct{})}
	main, err := owner.DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	window := time.Now().Add(250 * time.Millisecond)
	if err := owner.setMaintenance(window); err != nil {
		t.Fatal(err)
	}
	auxiliary, err := owner.DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer auxiliary.Close()
	if err := auxiliary.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(owner.connections) != 2 || !other.deadline.IsZero() || len(other.connections) != 0 {
		t.Fatal("physical sessions shared maintenance state")
	}
	done := make(chan error, 1)
	go func() { _, err := auxiliary.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal("fresh auxiliary socket did not inherit actual I/O bound", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fresh auxiliary socket retained pq's longer cancellation timeout")
	}
}

// A minimal database/sql holder exercises the public Raw discard contract. No
// SQL/PG protocol is simulated: real protocol blackholes live in service tests.
type maintenanceInvalidSession struct {
	postgresDriverConn
	closed atomic.Int64
}

func (s *maintenanceInvalidSession) IsValid() bool                      { return false }
func (s *maintenanceInvalidSession) ResetSession(context.Context) error { return driver.ErrBadConn }
func (s *maintenanceInvalidSession) Close() error                       { s.closed.Add(1); return nil }

type maintenanceHolderConnector struct{ conn driver.Conn }

func (c maintenanceHolderConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (maintenanceHolderConnector) Driver() driver.Driver                          { return maintenanceHolderDriver{} }

type maintenanceHolderDriver struct{}

func (maintenanceHolderDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unexpected legacy Open")
}

func TestPostgresMaintenanceRawDiscardsInvalidLeasedSessionWithoutClearingLateDialBound(t *testing.T) {
	base := &maintenanceInvalidSession{}
	window := time.Now().Add(-time.Second)
	owner := &postgresSockets{deadline: window, connections: make(map[*postgresSocket]struct{})}
	wrapped := &postgresConnection{postgresDriverConn: base, sockets: owner}
	db := sql.OpenDB(maintenanceHolderConnector{conn: wrapped})
	defer db.Close()
	leased, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	catalog := &Catalog{writer: leased}
	if err := catalog.setPostgresMaintenance(time.Time{}); !errors.Is(err, driver.ErrBadConn) {
		t.Fatal("invalid leased session was not rejected by Raw", err)
	}
	if !owner.deadline.Equal(window) || base.closed.Load() != 1 {
		t.Fatal("invalid session lost its late-dial bound or stayed physically open")
	}
	if err := leased.PingContext(t.Context()); !errors.Is(err, sql.ErrConnDone) {
		t.Fatal("leased connection silently recovered after invalidation", err)
	}
}

func TestPostgresMaintenanceReaderAcquisitionBoundsStartupBlackholes(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) { maintenanceReaderStartupBlackhole(t, mode) })
	}
}

func maintenanceReaderStartupBlackhole(t *testing.T, mode string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	started, release, serverDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		size := int64(binary.BigEndian.Uint32(header[:]))
		if size < 8 || size > 1<<20 {
			return
		}
		if _, err := io.CopyN(io.Discard, conn, size-4); err != nil {
			return
		}
		close(started)
		// A real pq StartupMessage reached this TCP endpoint. Deliberately send
		// no authentication/Ready response; release always closes the socket.
		<-release
	}()
	config, err := pq.NewConfig(fmt.Sprintf("host=127.0.0.1 port=%d dbname=fixture user=fixture sslmode=disable connect_timeout=0", listener.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(&postgresConnector{config: config})
	catalog := &Catalog{db: db, postgres: true}
	defer func() { unblock(); listener.Close(); db.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, closeReader, err := catalog.openDeadlineReader(ctx)
		if closeReader != nil {
			err = errors.Join(err, closeReader())
		}
		done <- err
	}()
	select {
	case <-started:
	case <-done:
		t.Fatal("connector returned before the startup-response fault boundary")
	case <-time.After(3 * time.Second):
		t.Fatal("test never reached the actual StartupMessage boundary")
	}
	if mode == "cancel" {
		cancel()
	}
	<-ctx.Done()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("startup blackhole produced a connection")
		}
	case <-time.After(750 * time.Millisecond):
		unblock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("connector did not stop after the diagnostic socket closed")
		}
		t.Fatal("reader acquisition startup ignored its context I/O deadline")
	}
	unblock()
	<-serverDone
}
