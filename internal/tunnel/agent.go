package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

const (
	initialReconnectDelay = time.Second
	maximumReconnectDelay = 30 * time.Second
)

// Dial connects an agent to the server and serves HTTP requests received over
// the yamux session until the connection or context is closed.
func Dial(ctx context.Context, url, token string, handler http.Handler) error {
	if handler == nil {
		return errors.New("tunnel handler is required")
	}
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return err
	}
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	session, err := yamux.Server(conn, nil)
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "tunnel setup failed")
		return err
	}
	listener := &streamTrackingListener{session: session, streams: make(map[*yamux.Stream]struct{})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- http.Serve(listener, handler) }()
	go func() {
		<-session.CloseChan()
		listener.interruptReads()
	}()

	select {
	case err := <-serveDone:
		_ = listener.Close()
		return err
	case <-session.CloseChan():
		listener.interruptReads()
		_ = listener.Close()
		return nil
	}
}

// streamTrackingListener interrupts active HTTP stream reads as soon as their
// yamux session shuts down. yamux v0.1.2 wakes Stream.Read on shutdown, then
// retries through its state lock; a past read deadline makes that retry return
// ErrTimeout instead of spinning while Session.Close waits for the same lock.
type streamTrackingListener struct {
	session *yamux.Session

	mu      sync.Mutex
	streams map[*yamux.Stream]struct{}
	close   sync.Once
}

func (l *streamTrackingListener) Accept() (net.Conn, error) {
	conn, err := l.session.Accept()
	if err != nil {
		return nil, err
	}
	stream, ok := conn.(*yamux.Stream)
	if !ok {
		return conn, nil
	}
	l.mu.Lock()
	l.streams[stream] = struct{}{}
	l.mu.Unlock()
	select {
	case <-l.session.CloseChan():
		_ = stream.SetReadDeadline(time.Now())
	default:
	}
	return &trackedStream{Stream: stream, listener: l}, nil
}

func (l *streamTrackingListener) Close() error {
	l.close.Do(func() {
		go func() { _ = l.session.Close() }()
	})
	return nil
}

func (l *streamTrackingListener) Addr() net.Addr { return l.session.Addr() }

func (l *streamTrackingListener) remove(stream *yamux.Stream) {
	l.mu.Lock()
	delete(l.streams, stream)
	l.mu.Unlock()
}

func (l *streamTrackingListener) interruptReads() {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline := time.Now()
	for stream := range l.streams {
		_ = stream.SetReadDeadline(deadline)
	}
}

type trackedStream struct {
	*yamux.Stream
	listener *streamTrackingListener
	close    sync.Once
}

func (s *trackedStream) Close() error {
	var err error
	s.close.Do(func() {
		s.listener.remove(s.Stream)
		err = s.Stream.Close()
	})
	return err
}

// RunForever dials the server and reconnects after a tunnel failure, backing
// off from one second up to thirty seconds between attempts.
func RunForever(ctx context.Context, url, token string, handler http.Handler) error {
	delay := initialReconnectDelay
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		_ = Dial(ctx, url, token, handler)
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(started) >= maximumReconnectDelay {
			delay = initialReconnectDelay
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if delay < maximumReconnectDelay {
			delay *= 2
			if delay > maximumReconnectDelay {
				delay = maximumReconnectDelay
			}
		}
	}
}
