package radix

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mediocregopher/radix/v4/internal/proc"
	"github.com/mediocregopher/radix/v4/resp"
)

// Conn is a Client wrapping a single network connection which synchronously
// reads/writes data using the redis resp protocol.
//
// A Conn can be used directly as a Client, but in general you probably want to
// use a *Pool instead
type Conn interface {
	// The Do method merely calls the Action's Perform method with the Conn as
	// the argument.
	Client

	// EncodeDecode will encode the given Marshaler onto the connection, then
	// decode a response into the given Unmarshaler. If either parameter is nil
	// then that step is skipped.
	EncodeDecode(context.Context, resp.Marshaler, resp.Unmarshaler) error

	// Returns the underlying network connection, as-is. Read, Write, and Close
	// should not be called on the returned Conn.
	NetConn() net.Conn
}

// ConnFunc is a function which returns an initialized, ready-to-be-used Conn.
// Functions like NewPool or NewCluster take in a ConnFunc in order to allow for
// things like calls to AUTH on each new connection, setting timeouts, custom
// Conn implementations, etc... See the package docs for more details.
type ConnFunc func(ctx context.Context, network, addr string) (Conn, error)

// DefaultConnFunc is a ConnFunc which will return a Conn for a redis instance
// using sane defaults.
var DefaultConnFunc = func(ctx context.Context, network, addr string) (Conn, error) {
	return Dial(ctx, network, addr)
}

// TODO what is this? is it needed?
func wrapDefaultConnFunc(addr string) ConnFunc {
	_, opts := parseRedisURL(addr)
	return func(ctx context.Context, network, addr string) (Conn, error) {
		return Dial(ctx, network, addr, opts...)
	}
}

////////////////////////////////////////////////////////////////////////////////

type connMarshalerUnmarshaler struct {
	ctx         context.Context
	marshaler   resp.Marshaler
	unmarshaler resp.Unmarshaler
	errCh       chan error
}

type conn struct {
	proc *proc.Proc

	net.Conn
	brw      *bufio.ReadWriter
	rCh, wCh chan connMarshalerUnmarshaler

	// errChPool is a buffered channel used as a makeshift pool of chan errors,s
	// owe don't have to make a new one on every EncodeDecode call.
	errChPool chan chan error
}

// NewConn takes an existing net.Conn and wraps it to support the Conn interface
// of this package. The Read and Write methods on the original net.Conn should
// not be used after calling this method.
func NewConn(netConn net.Conn) Conn {
	c := &conn{
		proc:      proc.New(),
		Conn:      netConn,
		brw:       bufio.NewReadWriter(bufio.NewReader(netConn), bufio.NewWriter(netConn)),
		rCh:       make(chan connMarshalerUnmarshaler, 128),
		wCh:       make(chan connMarshalerUnmarshaler, 128),
		errChPool: make(chan chan error, 16),
	}
	c.proc.Run(c.reader)
	c.proc.Run(c.writer)
	return c
}

func (c *conn) Close() error {
	return c.proc.PrefixedClose(c.Conn.Close, nil)
}

func (c *conn) writer(ctx context.Context) {
	doneCh := ctx.Done()
	for {
		select {
		case <-doneCh:
			return
		case mu := <-c.wCh:
			if mu.marshaler != nil {
				if err := mu.ctx.Err(); err != nil {
					mu.errCh <- err
					continue
				} else if deadline, ok := mu.ctx.Deadline(); ok {
					if err := c.Conn.SetWriteDeadline(deadline); err != nil {
						mu.errCh <- fmt.Errorf("setting write deadline to %v: %w", deadline, err)
						continue
					}
				} else if err := c.Conn.SetWriteDeadline(time.Time{}); err != nil {
					mu.errCh <- fmt.Errorf("unsetting write deadline: %w", err)
					continue
				}

				if err := mu.marshaler.MarshalRESP(c.brw.Writer); err != nil {
					mu.errCh <- err
					continue
				} else if err := c.brw.Writer.Flush(); err != nil {
					mu.errCh <- err
					continue
				}
			}

			// if there's no unmarshaler then don't forward to the reader
			if mu.unmarshaler == nil {
				mu.errCh <- nil
				continue
			}

			select {
			case <-doneCh:
				return
			case c.rCh <- mu:
			}
		}
	}
}

func (c *conn) reader(ctx context.Context) {
	doneCh := ctx.Done()
	for {
		select {
		case <-doneCh:
			return
		case mu := <-c.rCh:

			if mu.unmarshaler == nil {
				continue
			} else if err := mu.ctx.Err(); err != nil {
				mu.errCh <- err
				continue
			} else if deadline, ok := mu.ctx.Deadline(); ok {
				if err := c.Conn.SetReadDeadline(deadline); err != nil {
					mu.errCh <- fmt.Errorf("setting read deadline to %v: %w", deadline, err)
					continue
				}
			} else if err := c.Conn.SetReadDeadline(time.Time{}); err != nil {
				mu.errCh <- fmt.Errorf("unsetting read deadline: %w", err)
				continue
			}

			err := mu.unmarshaler.UnmarshalRESP(c.brw.Reader)
			if err != nil {
				// simplify things for the caller by translating network
				// timeouts into DeadlineExceeded, since that's actually what
				// happened.
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					err = context.DeadlineExceeded
				}
			}

			mu.errCh <- err
		}
	}
}

func (c *conn) getErrCh() chan error {
	select {
	case errCh := <-c.errChPool:
		return errCh
	default:
		return make(chan error, 1)
	}
}

func (c *conn) putErrCh(errCh chan error) {
	select {
	case c.errChPool <- errCh:
	default:
	}
}

func (c *conn) EncodeDecode(ctx context.Context, m resp.Marshaler, u resp.Unmarshaler) error {
	mu := connMarshalerUnmarshaler{
		ctx:         ctx,
		marshaler:   m,
		unmarshaler: u,
		errCh:       c.getErrCh(),
	}
	doneCh := ctx.Done()
	closedCh := c.proc.ClosedCh()

	select {
	case <-doneCh:
		return ctx.Err()
	case <-closedCh:
		return proc.ErrClosed
	case c.wCh <- mu:
	}

	select {
	case <-doneCh:
		return ctx.Err()
	case <-closedCh:
		return proc.ErrClosed
	case err := <-mu.errCh:
		// it's important that we only put the error channel back in the pool if
		// it's actually been used, otherwise it might still end up with
		// something written to it.
		c.putErrCh(mu.errCh)
		return err
	}
}

func (c *conn) Do(ctx context.Context, a Action) error {
	return a.Perform(ctx, c)
}

func (c *conn) NetConn() net.Conn {
	return c.Conn
}

////////////////////////////////////////////////////////////////////////////////

type dialOpts struct {
	authUser, authPass string
	selectDB           string
	useTLSConfig       bool
	tlsConfig          *tls.Config
}

// DialOpt is an optional behavior which can be applied to the Dial function to
// effect its behavior, or the behavior of the Conn it creates.
type DialOpt func(*dialOpts)

const defaultAuthUser = "default"

// DialAuthPass will cause Dial to perform an AUTH command once the connection
// is created, using the given pass.
//
// If this is set and a redis URI is passed to Dial which also has a password
// set, this takes precedence.
//
// Using DialAuthPass is equivalent to calling DialAuthUser with user "default"
// and is kept for compatibility with older package versions.
func DialAuthPass(pass string) DialOpt {
	return DialAuthUser(defaultAuthUser, pass)
}

// DialAuthUser will cause Dial to perform an AUTH command once the connection
// is created, using the given user and pass.
//
// If this is set and a redis URI is passed to Dial which also has a username
// and password set, this takes precedence.
func DialAuthUser(user, pass string) DialOpt {
	return func(do *dialOpts) {
		do.authUser = user
		do.authPass = pass
	}
}

// DialSelectDB will cause Dial to perform a SELECT command once the connection
// is created, using the given database index.
//
// If this is set and a redis URI is passed to Dial which also has a database
// index set, this takes precedence.
func DialSelectDB(db int) DialOpt {
	return func(do *dialOpts) {
		do.selectDB = strconv.Itoa(db)
	}
}

// DialUseTLS will cause Dial to perform a TLS handshake using the provided
// config. If config is nil the config is interpreted as equivalent to the zero
// configuration. See https://golang.org/pkg/crypto/tls/#Config
func DialUseTLS(config *tls.Config) DialOpt {
	return func(do *dialOpts) {
		do.tlsConfig = config
		do.useTLSConfig = true
	}
}

func parseRedisURL(urlStr string) (string, []DialOpt) {
	// do a quick check before we bust out url.Parse, in case that is very
	// unperformant
	if !strings.HasPrefix(urlStr, "redis://") {
		return urlStr, nil
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return urlStr, nil
	}

	q := u.Query()

	username := defaultAuthUser
	if n := u.User.Username(); n != "" {
		username = n
	} else if n := q.Get("username"); n != "" {
		username = n
	}

	password := q.Get("password")
	if p, ok := u.User.Password(); ok {
		password = p
	}

	opts := []DialOpt{
		DialAuthUser(username, password),
	}

	dbStr := q.Get("db")
	if u.Path != "" && u.Path != "/" {
		dbStr = u.Path[1:]
	}

	if dbStr, err := strconv.Atoi(dbStr); err == nil {
		opts = append(opts, DialSelectDB(dbStr))
	}

	return u.Host, opts
}

// Dial is a ConnFunc which creates a Conn using net.Dial and NewConn. It takes
// in a number of options which can overwrite its default behavior as well.
//
// In place of a host:port address, Dial also accepts a URI, as per:
// 	https://www.iana.org/assignments/uri-schemes/prov/redis
// If the URI has an AUTH password or db specified Dial will attempt to perform
// the AUTH and/or SELECT as well.
//
// If either DialAuthPass or DialSelectDB is used it overwrites the associated
// value passed in by the URI.
//
// The default options Dial uses are:
//
//	DialTimeout(10 * time.Second)
//
func Dial(ctx context.Context, network, addr string, opts ...DialOpt) (Conn, error) {
	var do dialOpts
	addr, addrOpts := parseRedisURL(addr)
	for _, opt := range addrOpts {
		opt(&do)
	}
	for _, opt := range opts {
		opt(&do)
	}
	
	var netConn net.Conn
	var err error
	dialer := net.Dialer{}
	if do.useTLSConfig {
		netConn, err = tls.DialWithDialer(&dialer, network, addr, do.tlsConfig)
	} else {
		netConn, err = dialer.Dial(network, addr)
	}
	
	if err != nil {
		return nil, err
	}

	// If the netConn is a net.TCPConn (or some wrapper for it) and so can have
	// keepalive enabled, do so with a sane (though slightly aggressive)
	// default.
	{
		type keepaliveConn interface {
			SetKeepAlive(bool) error
			SetKeepAlivePeriod(time.Duration) error
		}

		if kaConn, ok := netConn.(keepaliveConn); ok {
			if err = kaConn.SetKeepAlive(true); err != nil {
				netConn.Close()
				return nil, err
			} else if err = kaConn.SetKeepAlivePeriod(10 * time.Second); err != nil {
				netConn.Close()
				return nil, err
			}
		}
	}

	conn := NewConn(netConn)

	if do.authUser != "" && do.authUser != defaultAuthUser {
		if err := conn.Do(ctx, Cmd(nil, "AUTH", do.authUser, do.authPass)); err != nil {
			conn.Close()
			return nil, err
		}
	} else if do.authPass != "" {
		if err := conn.Do(ctx, Cmd(nil, "AUTH", do.authPass)); err != nil {
			conn.Close()
			return nil, err
		}
	}

	if do.selectDB != "" {
		if err := conn.Do(ctx, Cmd(nil, "SELECT", do.selectDB)); err != nil {
			conn.Close()
			return nil, err
		}
	}

	return conn, nil
}
