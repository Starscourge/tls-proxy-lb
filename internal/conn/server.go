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
// Each listener has its own Handler, so one Server can run HTTPS, plain HTTP
// and redirect listeners under one connection cap.
//
// Goroutine ownership: each Serve call is one accept loop, stopped by
// CloseListener, Shutdown or Close closing its listener. Each accepted
// connection gets one goroutine, registered in the Tracker and stopped by
// its handler returning or by Close closing the connection. Shutdown and
// Close wait for both kinds to finish.
type Server struct {
	tracker *Tracker
	logger  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// listeners maps each served listener to whether CloseListener has
	// stopped it.
	listeners map[net.Listener]bool
	closed    bool
	loops     sync.WaitGroup
}

// NewServer returns a server that enforces limits across all its listeners
// (see Limits). A nil logger discards.
func NewServer(limits Limits, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		tracker:   NewTracker(limits),
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		listeners: make(map[net.Listener]bool),
	}
}

// Tracker returns the server's connection tracker.
func (s *Server) Tracker() *Tracker { return s.tracker }

// ErrServerClosed is returned by Serve after Close.
var ErrServerClosed = errors.New("conn: server closed")

// Serve accepts connections on ln and hands each to h until CloseListener,
// Shutdown or Close, then returns ErrServerClosed. It takes ownership of
// ln. Any other return is an accept error that ended the loop; ln is closed
// in every case.
func (s *Server) Serve(ln net.Listener, h Handler) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return ErrServerClosed
	}
	s.listeners[ln] = false
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
			if s.stopped(ln) {
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
		tc, reason := s.tracker.Add(c)
		switch reason {
		case RejectNone:
		case RejectPerIP:
			// One client at its own cap is routine under abuse, and
			// counted in the metrics; logging each refusal would let it
			// flood the log.
			s.logger.Debug("connection refused: at per-IP connection cap", "remote", c.RemoteAddr().String())
			c.Close()
			continue
		default:
			// Over the cap (or closing): refuse at once rather than queue.
			s.logger.Warn("connection refused: at connection cap", "remote", c.RemoteAddr().String())
			c.Close()
			continue
		}
		go s.serveConn(tc, h)
	}
}

func (s *Server) serveConn(tc *TrackedConn, h Handler) {
	defer s.tracker.Remove(tc)
	defer tc.Close()
	h.ServeConn(s.ctx, tc)
}

// stopped reports whether ln's accept loop was told to end.
func (s *Server) stopped(ln net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || s.listeners[ln]
}

// CloseListener stops accepting on ln, which Serve is serving; that Serve
// returns ErrServerClosed. Connections already accepted on ln carry on. It
// reports whether ln was being served. A config reload uses it to close a
// port that was removed.
func (s *Server) CloseListener(ln net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.listeners[ln]; !ok {
		return false
	}
	s.listeners[ln] = true
	ln.Close()
	return true
}

// closeListeners marks the server closed, so Serve refuses new listeners,
// closes every listener and waits for every accept loop to return. After it
// no connection is added to the tracker.
func (s *Server) closeListeners() {
	s.mu.Lock()
	s.closed = true
	for ln := range s.listeners {
		ln.Close()
	}
	s.mu.Unlock()
	s.loops.Wait()
}

// Shutdown stops accepting, then lets open connections finish: idle ones
// are closed at once, and the rest finish the request they are on with
// Connection: close (see Tracker.Drain). It returns nil once every
// connection has ended. If ctx ends first, it closes what remains as Close
// does and returns ctx's error.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeListeners()
	s.tracker.Drain()
	done := s.tracker.Done()
	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
	}
	s.Close()
	<-done
	return ctx.Err()
}

// Close stops every accept loop, closes every open connection and waits for
// all of their goroutines to return. In-flight requests are cut off;
// Shutdown is the graceful way.
func (s *Server) Close() error {
	s.closeListeners()
	s.cancel()
	s.tracker.CloseAll()
	s.tracker.Wait()
	return nil
}
