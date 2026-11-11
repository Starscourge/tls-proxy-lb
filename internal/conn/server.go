package conn

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// acceptRetryDelay is the pause after an Accept error other than a closed
// listener, so a persistent error cannot spin the loop. Phase 8 replaces it
// with a backoff for running out of file descriptors.
const acceptRetryDelay = 10 * time.Millisecond

// Handler serves one client connection. ServeConn returns when the
// connection is finished; the Server closes it afterwards. ctx is cancelled
// when the Server closes.
type Handler interface {
	ServeConn(ctx context.Context, c *TrackedConn)
}

// Server runs accept loops and one goroutine per client connection.
//
// Goroutine ownership: each Serve call is one accept loop, stopped by Close
// closing its listener. Each accepted connection gets one goroutine,
// registered in the Tracker and stopped by its handler returning or by Close
// closing the connection. Close waits for both kinds to finish.
type Server struct {
	handler Handler
	tracker *Tracker
	logger  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	listeners map[net.Listener]struct{}
	closed    bool
	loops     sync.WaitGroup
}

// NewServer returns a server that hands connections to h and admits at most
// maxConns at once (no cap if <= 0). A nil logger discards.
func NewServer(h Handler, maxConns int, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		handler:   h,
		tracker:   NewTracker(maxConns),
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		listeners: make(map[net.Listener]struct{}),
	}
}

// Tracker returns the server's connection tracker.
func (s *Server) Tracker() *Tracker { return s.tracker }

// ErrServerClosed is returned by Serve after Close.
var ErrServerClosed = errors.New("conn: server closed")

// Serve accepts connections on ln until Close, then returns ErrServerClosed.
// It takes ownership of ln. Any other return is an accept error that ended
// the loop; ln is closed in every case.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return ErrServerClosed
	}
	s.listeners[ln] = struct{}{}
	s.loops.Add(1)
	s.mu.Unlock()
	defer s.loops.Done()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, ln)
		s.mu.Unlock()
		ln.Close()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return ErrServerClosed
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			s.logger.Warn("accept failed", "listener", ln.Addr().String(), "err", err)
			select {
			case <-time.After(acceptRetryDelay):
			case <-s.ctx.Done():
				return ErrServerClosed
			}
			continue
		}
		tc, ok := s.tracker.Add(c)
		if !ok {
			// Over the cap (or closing): refuse at once rather than queue.
			s.logger.Warn("connection refused: at connection cap", "remote", c.RemoteAddr().String())
			c.Close()
			continue
		}
		go s.serveConn(tc)
	}
}

func (s *Server) serveConn(tc *TrackedConn) {
	defer s.tracker.Remove(tc)
	defer tc.Close()
	s.handler.ServeConn(s.ctx, tc)
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close stops every accept loop, closes every open connection and waits for
// all of their goroutines to return. In-flight requests are cut off; the
// graceful drain is Phase 7.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	for ln := range s.listeners {
		ln.Close()
	}
	s.mu.Unlock()
	s.cancel()
	s.loops.Wait()
	s.tracker.CloseAll()
	s.tracker.Wait()
	return nil
}
