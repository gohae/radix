package radix

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	. "testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gohae/radix/v4/internal/proc"
	"github.com/gohae/radix/v4/resp"
	"github.com/gohae/radix/v4/resp/resp3"
	"github.com/gohae/radix/v4/trace"
)

func testPoolWithTrace(t *T, size int, poolTrace trace.PoolTrace, opts ...PoolOpt) *Pool {
	initDoneCh := make(chan struct{})
	prevInitCompleted := poolTrace.InitCompleted
	poolTrace.InitCompleted = func(pic trace.PoolInitCompleted) {
		if prevInitCompleted != nil {
			prevInitCompleted(pic)
		}
		close(initDoneCh)
	}
	opts = append(opts, PoolWithTrace(poolTrace))

	ctx := testCtx(t)
	pool, err := NewPool(ctx, "tcp", "localhost:6379", size, opts...)
	if err != nil {
		t.Fatal(err)
	}
	<-initDoneCh
	t.Cleanup(func() {
		pool.Close()
	})
	return pool
}

func testPool(t *T, size int, opts ...PoolOpt) *Pool {
	return testPoolWithTrace(t, size, trace.PoolTrace{}, opts...)
}

func TestPool(t *T) {

	testEcho := func(ctx context.Context, c Conn) error {
		exp := randStr()
		var out string
		assert.Nil(t, c.Do(ctx, Cmd(&out, "ECHO", exp)))
		assert.Equal(t, exp, out)
		return nil
	}

	doWithTrace := func(t *T, poolTrace trace.PoolTrace, opts ...PoolOpt) {
		ctx := testCtx(t)
		opts = append(opts, PoolOnFullClose())
		size := 10
		pool := testPoolWithTrace(t, size, poolTrace, opts...)
		var wg sync.WaitGroup
		for i := 0; i < size*4; i++ {
			wg.Add(1)
			go func() {
				for i := 0; i < 100; i++ {
					assert.NoError(t, pool.Do(ctx, WithConn("", testEcho)))
				}
				wg.Done()
			}()
		}
		wg.Wait()
		assert.Equal(t, size, pool.NumAvailConns())
		pool.Close()
		assert.Equal(t, 0, pool.NumAvailConns())
	}

	do := func(t *T, opts ...PoolOpt) {
		doWithTrace(t, trace.PoolTrace{}, opts...)
	}

	t.Run("onEmptyWait", func(t *T) { do(t, PoolOnEmptyWait()) })
	t.Run("onEmptyCreate", func(t *T) { do(t, PoolOnEmptyCreateAfter(0)) })
	t.Run("onEmptyCreateAfter", func(t *T) { do(t, PoolOnEmptyCreateAfter(1*time.Second)) })
	// This one is expected to error, since this test empties the pool by design
	//t.Run("onEmptyErr", func(t *T) { do(PoolOnEmptyErrAfter(0)) })
	t.Run("onEmptyErrAfter", func(t *T) { do(t, PoolOnEmptyErrAfter(1*time.Second)) })

	t.Run("withTrace", func(t *T) {
		var connCreatedCount int
		var connClosedCount int
		var initializedAvailCount int
		doWithTrace(t, trace.PoolTrace{
			ConnCreated: func(done trace.PoolConnCreated) {
				connCreatedCount++
			},
			ConnClosed: func(closed trace.PoolConnClosed) {
				connClosedCount++
			},
			InitCompleted: func(completed trace.PoolInitCompleted) {
				initializedAvailCount = completed.AvailCount
			},
		})
		if initializedAvailCount != 10 {
			t.Fail()
		}
		if connCreatedCount != connClosedCount {
			t.Fail()
		}
	})
}

// Test all the different OnEmpty behaviors
func TestPoolGet(t *T) {
	ctx := testCtx(t)
	getBlock := func(p *Pool) (time.Duration, error) {
		start := time.Now()
		_, err := p.get(ctx)
		return time.Since(start), err
	}

	// this one is a bit weird, cause it would block infinitely if we let it
	t.Run("onEmptyWait", func(t *T) {
		pool := testPool(t, 1, PoolOnEmptyWait())
		conn, err := pool.get(ctx)
		assert.NoError(t, err)

		go func() {
			time.Sleep(2 * time.Second)
			pool.put(conn)
		}()
		took, err := getBlock(pool)
		assert.NoError(t, err)
		assert.True(t, took-2*time.Second < 20*time.Millisecond)
	})

	// the rest are pretty straightforward
	gen := func(mkOpt func(time.Duration) PoolOpt, d time.Duration, expErr error) func(*T) {
		return func(t *T) {
			pool := testPool(t, 0, PoolOnFullClose(), mkOpt(d))
			took, err := getBlock(pool)
			assert.Equal(t, expErr, err)
			assert.True(t, took-d < 20*time.Millisecond)
		}
	}

	t.Run("onEmptyCreate", gen(PoolOnEmptyCreateAfter, 0, nil))
	t.Run("onEmptyCreateAfter", gen(PoolOnEmptyCreateAfter, 1*time.Second, nil))
	t.Run("onEmptyErr", gen(PoolOnEmptyErrAfter, 0, ErrPoolEmpty))
	t.Run("onEmptyErrAfter", gen(PoolOnEmptyErrAfter, 1*time.Second, ErrPoolEmpty))
}

func TestPoolOnFull(t *T) {
	t.Run("onFullClose", func(t *T) {
		ctx := testCtx(t)
		var reason trace.PoolConnClosedReason
		pool := testPoolWithTrace(t, 1,
			trace.PoolTrace{ConnClosed: func(c trace.PoolConnClosed) {
				reason = c.Reason
			}},
			PoolOnFullClose(),
		)
		defer pool.Close()
		assert.Equal(t, 1, len(pool.pool))

		spc, err := pool.newConn(ctx, "TEST")
		assert.NoError(t, err)
		pool.put(spc)
		assert.Equal(t, 1, len(pool.pool))
		assert.Equal(t, trace.PoolConnClosedReasonPoolFull, reason)
	})

	t.Run("onFullBuffer", func(t *T) {
		ctx := testCtx(t)
		pool := testPool(t, 1, PoolOnFullBuffer(1, 1*time.Second))
		defer pool.Close()
		assert.Equal(t, 1, len(pool.pool))

		// putting a conn should overflow
		spc, err := pool.newConn(ctx, "TEST")
		assert.NoError(t, err)
		pool.put(spc)
		assert.Equal(t, 2, len(pool.pool))

		// another shouldn't, overflow is full
		spc, err = pool.newConn(ctx, "TEST")
		assert.NoError(t, err)
		pool.put(spc)
		assert.Equal(t, 2, len(pool.pool))

		// retrieve from the pool, drain shouldn't do anything because the
		// overflow is empty now
		<-pool.pool
		assert.Equal(t, 1, len(pool.pool))
		time.Sleep(2 * time.Second)
		assert.Equal(t, 1, len(pool.pool))

		// if both are full then drain should remove the overflow one
		spc, err = pool.newConn(ctx, "TEST")
		assert.NoError(t, err)
		pool.put(spc)
		assert.Equal(t, 2, len(pool.pool))
		time.Sleep(2 * time.Second)
		assert.Equal(t, 1, len(pool.pool))
	})
}

func TestPoolPut(t *T) {
	ctx := testCtx(t)
	size := 10
	pool := testPool(t, size)

	assertPoolConns := func(exp int) {
		assert.Equal(t, exp, pool.NumAvailConns())
	}
	assertPoolConns(10)

	// Make sure that put does not accept a connection which has had a critical
	// network error
	pool.Do(ctx, WithConn("", func(ctx context.Context, conn Conn) error {
		assertPoolConns(9)
		conn.(*ioErrConn).lastIOErr = io.EOF
		return nil
	}))
	assertPoolConns(9)

	// Make sure that a put _does_ accept a connection which had an unmarshal
	// error
	pool.Do(ctx, WithConn("", func(ctx context.Context, conn Conn) error {
		var i int
		assert.NotNil(t, conn.Do(ctx, Cmd(&i, "ECHO", "foo")))
		assert.Nil(t, conn.(*ioErrConn).lastIOErr)
		return nil
	}))
	assertPoolConns(9)

	// Make sure that a put _does_ accept a connection which had an app level
	// resp error
	pool.Do(ctx, WithConn("", func(ctx context.Context, conn Conn) error {
		assert.NotNil(t, Cmd(nil, "CMDDNE"))
		assert.Nil(t, conn.(*ioErrConn).lastIOErr)
		return nil
	}))
	assertPoolConns(9)

	// Make sure that closing the pool closes outstanding connections as well
	closeCh := make(chan bool)
	go func() {
		<-closeCh
		assert.Nil(t, pool.Close())
		closeCh <- true
	}()
	pool.Do(ctx, WithConn("", func(ctx context.Context, conn Conn) error {
		closeCh <- true
		<-closeCh
		return nil
	}))
	assertPoolConns(0)
}

// TestPoolDoDoesNotBlock checks that with a positive onEmptyWait Pool.Do()
// does not block longer than the timeout period given by user
func TestPoolDoDoesNotBlock(t *T) {
	ctx := testCtx(t)
	size := 10
	requestTimeout := 200 * time.Millisecond
	redialInterval := 100 * time.Millisecond

	connFunc := PoolConnFunc(func(context.Context, string, string) (Conn, error) {
		return dial(), nil
	})
	pool := testPool(t, size,
		PoolOnEmptyCreateAfter(redialInterval),
		connFunc,
	)

	assertPoolConns := func(exp int) {
		assert.Equal(t, exp, pool.NumAvailConns())
	}
	assertPoolConns(size)

	var wg sync.WaitGroup
	var timeExceeded uint32

	// here we try to imitate external requests which come one at a time
	// and exceed the number of connections in pool
	for i := 0; i < 5*size; i++ {
		wg.Add(1)
		go func(i int) {
			time.Sleep(time.Duration(i) * 10 * time.Millisecond)

			timeStart := time.Now()
			pool.Do(ctx, WithConn("", func(ctx context.Context, conn Conn) error {
				time.Sleep(requestTimeout)
				conn.(*ioErrConn).lastIOErr = errors.New("i/o timeout")
				return nil
			}))

			if time.Since(timeStart)-requestTimeout-redialInterval > 20*time.Millisecond {
				atomic.AddUint32(&timeExceeded, 1)
			}
			wg.Done()
		}(i)
	}

	wg.Wait()
	assert.True(t, timeExceeded == 0)
}

func TestPoolClose(t *T) {
	ctx := testCtx(t)
	pool := testPool(t, 1)
	assert.NoError(t, pool.Do(ctx, Cmd(nil, "PING")))
	assert.NoError(t, pool.Close())
	assert.Error(t, proc.ErrClosed, pool.Do(ctx, Cmd(nil, "PING")))
}

func TestIoErrConn(t *T) {
	t.Run("NotReusableAfterError", func(t *T) {
		ctx := testCtx(t)
		dummyError := errors.New("i am error")

		ioc := newIOErrConn(NewStubConn("tcp", "127.0.0.1:6379", nil))
		ioc.lastIOErr = dummyError

		require.Equal(t, dummyError, ioc.EncodeDecode(ctx, &resp3.Any{}, &resp3.Any{}))
		require.Nil(t, ioc.Close())
	})

	t.Run("ReusableAfterRESPError", func(t *T) {
		ctx := testCtx(t)
		ioc := newIOErrConn(dial())
		defer ioc.Close()

		err1 := ioc.Do(ctx, Cmd(nil, "EVAL", "Z", "0"))
		require.True(t, errors.As(err1, new(resp.ErrConnUsable)))
		require.True(t, errors.As(err1, new(resp3.SimpleError)))

		err2 := ioc.Do(ctx, Cmd(nil, "GET", randStr()))
		require.Nil(t, err2)
	})
}
