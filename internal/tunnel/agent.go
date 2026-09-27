package tunnel

import (
	"context"
	"errors"
	"net/http"
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
	defer session.Close()
	return http.Serve(session, handler)
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
