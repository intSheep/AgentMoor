package platform

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Keep pq's full connection surface. In particular, silently falling back to
// database/sql's legacy argument or transaction paths would change semantics.
// Prepared statements are returned unchanged by the embedded pq implementation.
type postgresDriverConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.Execer
	driver.ExecerContext
	driver.Queryer
	driver.QueryerContext
	driver.NamedValueChecker
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type postgresConnector struct{ config pq.Config }
type postgresMaintenanceConnectContext struct{}

func (c *postgresConnector) Driver() driver.Driver { return &pq.Driver{} }
func (c *postgresConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connector, err := pq.NewConnectorConfig(c.config.Clone())
	if err != nil {
		return nil, err
	}
	// Each physical session owns its dialer, including pq's cancellation sockets.
	// Read-pool sessions never share the leased writer's maintenance window.
	sockets := &postgresSockets{connections: make(map[*postgresSocket]struct{})}
	if deadline, ok := ctx.Value(postgresMaintenanceConnectContext{}).(time.Time); ok {
		sockets.deadline = deadline
		// pq startup/TLS/authentication does not watch cancellation itself.
		// Only a new maintenance reader opts in; ordinary connections retain
		// pq's configured behavior. Join this small deadline callback on exit.
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = sockets.setMaintenance(time.Now())
			close(done)
		})
		defer func() {
			if !stop() {
				<-done
			}
		}()
	}
	connector.Dialer(sockets)
	conn, err := connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	complete, ok := conn.(postgresDriverConn)
	if !ok {
		conn.Close()
		return nil, errors.New("PostgreSQL driver connection interface changed")
	}
	return &postgresConnection{postgresDriverConn: complete, sockets: sockets}, nil
}

type postgresConnection struct {
	postgresDriverConn
	sockets *postgresSockets
}

// Only Raw accesses the driver wrapper; no driver.Conn escapes that callback.
func (c *Catalog) setPostgresMaintenance(deadline time.Time) error {
	conn, ok := c.writer.(*sql.Conn)
	if !ok {
		return errors.New("PostgreSQL leased writer is unavailable")
	}
	return setPostgresMaintenance(conn, deadline)
}

func setPostgresMaintenance(conn *sql.Conn, deadline time.Time) error {
	return conn.Raw(func(raw any) error {
		owned, ok := raw.(*postgresConnection)
		if !ok {
			return errors.New("PostgreSQL leased writer has no owned transport")
		}
		if !owned.IsValid() {
			// Leave an expired window in place for a late pq cancel dial.
			return driver.ErrBadConn
		}
		if err := owned.sockets.setMaintenance(deadline); err != nil {
			return errors.Join(driver.ErrBadConn, err)
		}
		return nil
	})
}

type postgresSockets struct {
	mu          sync.Mutex
	deadline    time.Time
	connections map[*postgresSocket]struct{}
}

func (s *postgresSockets) Dial(network, address string) (net.Conn, error) {
	return s.DialContext(context.Background(), network, address)
}
func (s *postgresSockets) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.DialContext(ctx, network, address)
}
func (s *postgresSockets) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	s.mu.Lock()
	deadline := s.deadline
	s.mu.Unlock()
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	owned := &postgresSocket{Conn: conn, owner: s}
	// The window may have changed while Dial was in progress.
	if err := owned.applyLocked(); err != nil {
		conn.Close()
		return nil, err
	}
	s.connections[owned] = struct{}{}
	return owned, nil
}
func (s *postgresSockets) setMaintenance(deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deadline.IsZero() && !s.deadline.IsZero() && !time.Now().Before(s.deadline) {
		// A genuine I/O fuse cannot be turned into a reusable healthy socket.
		// pq's cancellation goroutine may not have started its auxiliary Dial yet.
		return driver.ErrBadConn
	}
	s.deadline = deadline
	var result error
	for conn := range s.connections {
		result = errors.Join(result, conn.applyLocked())
	}
	return result
}

type postgresSocket struct {
	net.Conn
	owner       *postgresSockets
	read, write time.Time // deadlines requested by pq, restored after maintenance
}

func earlierDeadline(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
func (s *postgresSocket) applyLocked() error {
	return errors.Join(
		s.Conn.SetReadDeadline(earlierDeadline(s.read, s.owner.deadline)),
		s.Conn.SetWriteDeadline(earlierDeadline(s.write, s.owner.deadline)),
	)
}
func (s *postgresSocket) SetDeadline(deadline time.Time) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.read, s.write = deadline, deadline
	return s.applyLocked()
}
func (s *postgresSocket) SetReadDeadline(deadline time.Time) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.read = deadline
	return s.Conn.SetReadDeadline(earlierDeadline(deadline, s.owner.deadline))
}
func (s *postgresSocket) SetWriteDeadline(deadline time.Time) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.write = deadline
	return s.Conn.SetWriteDeadline(earlierDeadline(deadline, s.owner.deadline))
}
func (s *postgresSocket) Close() error {
	s.owner.mu.Lock()
	delete(s.owner.connections, s)
	s.owner.mu.Unlock()
	return s.Conn.Close()
}
