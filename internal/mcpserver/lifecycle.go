package mcpserver

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

// The server's lifecycle (design 5.5 to 5.8): the root context that every
// handler's context derives from, the slots that bound how many calls run
// at once, each call's budget, and the one place that starts a goroutine.

// safeGo runs f in a goroutine of its own, the only way the server starts
// one, and sends what it returns, or the panic it recovered as an error,
// on the channel it returns, which has room for it. name says what f is,
// for the error.
func safeGo(ctx context.Context, name string, f func(ctx context.Context) error) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("%s panicked: %v\n%s", name, p, debug.Stack())
			}
		}()
		done <- f(ctx)
	}()
	return done
}

// rootContext returns ctx, a request's context, cut also by the root
// context, so that shutting down ends every running handler at once: the
// SDK cancels a request's context when the client cancels it or its input
// ends, but not on a signal.
func (s *Server) rootContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.root, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// begin marks a call as running until the function it returns is called,
// so that Close can wait for the calls to end.
func (s *Server) begin() func() {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls--
		if s.calls == 0 && s.drained != nil {
			close(s.drained)
			s.drained = nil
		}
	}
}

// takeSlot waits for one of the call slots, at most the slot wait or until
// ctx ends, and returns the function that gives it back. It fails with
// busy when every slot stayed taken.
func (s *Server) takeSlot(ctx context.Context) (func(), error) {
	release := func() { <-s.slots }
	select {
	case s.slots <- struct{}{}:
		return release, nil
	default:
	}
	wait := time.NewTimer(s.o.SlotWait)
	defer wait.Stop()
	select {
	case s.slots <- struct{}{}:
		return release, nil
	case <-wait.C:
	case <-ctx.Done():
	}
	return nil, &toolErr{code: codeBusy, msg: s.mask.compose("%d calls are already running; try again in a moment.", s.o.MaxCalls)}
}

// reserve is how much of a call's budget a walk keeps back: it begins no
// further request with less than this left, and answers with what it has.
func (c *call) reserve() time.Duration {
	return min(10*time.Second, c.budget/4)
}

// roomForMore reports whether the call has time left for another request
// beyond its reserve.
func (c *call) roomForMore() bool {
	return time.Until(c.deadline) > c.reserve()
}

// closeWait is how long Close waits for running calls, and then for the
// store's reader to close.
const closeWait = 200 * time.Millisecond

// Close ends the root context, so that every running handler ends, waits
// at most 200 ms for them, and closes the store's reader, waiting at most
// 200 ms for that, and logging what went wrong. A call that comes
// afterwards finds its context ended.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.cancelRoot()
		s.mu.Lock()
		var drained chan struct{}
		if s.calls > 0 {
			drained = make(chan struct{})
			s.drained = drained
		}
		s.mu.Unlock()
		if drained != nil {
			select {
			case <-drained:
			case <-time.After(closeWait):
			}
		}
		closed := safeGo(context.Background(), "closing the store", func(context.Context) error { return s.store.close() })
		select {
		case s.closeErr = <-closed:
			if s.closeErr != nil {
				s.log.Warn("the store's reader did not close cleanly", "component", "store", "error", s.closeErr)
			}
		case <-time.After(closeWait):
			s.log.Warn("the store's reader took more than 200 ms to close; the server stops without waiting")
		}
	})
	return s.closeErr
}
