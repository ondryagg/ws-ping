package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/ondryadev/ws-ping/probe/internal/check"
	"github.com/ondryadev/ws-ping/probe/internal/metrics"
	"github.com/ondryadev/ws-ping/probe/internal/proto"
)

// handleWS runs the pre-upgrade checks in spec order (cheap checks first,
// nothing allocated until they pass), then owns the connection for the whole
// session.
func (s *Server) handleWS(hw *hijackWriter, r *http.Request) {
	key, internal, ok := s.client(r)
	if !ok {
		s.reject(hw, http.StatusBadRequest, metrics.ReasonBadRequest)
		return
	}

	if !internal {
		if s.unavailable(hw) {
			return
		}
		// 1. Capacity. Do not accept work that cannot be finished.
		if !s.sem.TryAcquire() {
			s.reject(hw, http.StatusServiceUnavailable, metrics.ReasonCapacity)
			return
		}
		defer s.sem.Release()
		if !s.perIP.TryAcquire(key) {
			s.reject(hw, http.StatusTooManyRequests, metrics.ReasonPerIP)
			return
		}
		defer s.perIP.Release(key)
		if !s.newConn.Allow(key) {
			s.reject(hw, http.StatusTooManyRequests, metrics.ReasonRateLimit)
			return
		}
	}

	// 2. Host.
	if !check.HostMatches(r.Host, s.cfg.Hostname) {
		s.reject(hw, http.StatusMisdirectedRequest, metrics.ReasonHost)
		return
	}
	// 3. Method (the path was matched by the router).
	if r.Method != http.MethodGet {
		hw.Header().Set("Allow", http.MethodGet)
		s.reject(hw, http.StatusMethodNotAllowed, metrics.ReasonMethod)
		return
	}
	// 4. Origin: exact match, exactly one header. WebSockets are not covered
	// by CORS, so this check is the protection.
	if o := r.Header.Values("Origin"); len(o) != 1 || !s.origins.Allowed(o[0]) {
		s.reject(hw, http.StatusForbidden, metrics.ReasonOrigin)
		return
	}
	// 5. Subprotocol.
	if !check.OffersSubprotocol(r.Header.Values("Sec-WebSocket-Protocol"), proto.Subprotocol) {
		s.reject(hw, http.StatusBadRequest, metrics.ReasonSubprotocol)
		return
	}

	if !s.beginSession() {
		s.reject(hw, http.StatusServiceUnavailable, metrics.ReasonCapacity)
		return
	}
	defer s.endSession()

	if s.hooks.OnUpgrade != nil {
		s.hooks.OnUpgrade(s.base)
	}
	c, err := websocket.Accept(hw, r, &websocket.AcceptOptions{
		Subprotocols:    []string{proto.Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
		// The library's own origin check is replaced by the exact match above.
		InsecureSkipVerify: true,
	})
	if err != nil || hw.conn == nil {
		s.errs.Add(s.now(), "upgrade", errString(err))
		if !internal {
			s.m.ConnsTotal.WithLabelValues(metrics.ResultUpgradeFail).Inc()
		}
		return
	}
	s.session(c, hw.conn, internal, r.RemoteAddr)
}

func errString(err error) string {
	if err == nil {
		return "no hijacked connection"
	}
	return err.Error()
}

// session runs one WebSocket session in the calling goroutine. Owner: the
// HTTP handler goroutine. Exit: every path below ends in a bounded close.
func (s *Server) session(c *websocket.Conn, nc net.Conn, internal bool, remote string) {
	start := time.Now()
	end := start.Add(s.cfg.SessionLifetime)
	// The "context that cancels everything": a backstop in case a net
	// deadline fails to fire. Net deadlines expire first.
	ctx, cancel := context.WithDeadline(s.base, end.Add(s.cfg.CloseTimeout+failsafeSlack))
	defer cancel()

	if !internal {
		s.m.ConnsActive.Inc()
	}
	result := metrics.ResultPanic // overwritten unless the loop panics
	var cause error
	if !internal && s.log.Enabled(s.base, slog.LevelDebug) {
		s.log.Debug("session open", "remote", remote)
		defer func() {
			s.log.Debug("session closed", "remote", remote, "result", result, "err", cause,
				"duration", time.Since(start).String())
		}()
	}
	defer func() {
		_ = c.CloseNow()
		_ = nc.Close()
		if !internal {
			s.m.ConnsActive.Dec()
			s.m.ConnsTotal.WithLabelValues(result).Inc()
			s.m.SessionDuration.Observe(time.Since(start).Seconds())
		}
	}()
	result, cause = s.loop(ctx, c, nc, end, internal)
}

func (s *Server) loop(ctx context.Context, c *websocket.Conn, nc net.Conn, end time.Time, internal bool) (string, error) {
	c.SetReadLimit(int64(s.cfg.FrameMax))
	buf := make([]byte, 0, 128)

	buf = s.hello.Append(buf, s.now().UnixMilli())
	if err := s.writeFrame(ctx, c, nc, buf); err != nil {
		return s.writeFailed(err), err
	}

	for pings := 0; pings < s.cfg.MaxPings; {
		dl := time.Now().Add(s.cfg.ReadTimeout)
		lifetime := !dl.Before(end)
		if lifetime {
			dl = end
		}
		typ, data, err := s.readFrame(ctx, c, nc, dl)
		if err != nil {
			return s.readFailed(c, nc, err, lifetime), err
		}
		if typ != websocket.MessageText || len(data) > s.cfg.FrameMax {
			s.closeWith(c, nc, websocket.StatusMessageTooBig, "text frames of 64 bytes at most")
			return metrics.ResultTooBig, nil
		}
		seq, perr := proto.ParsePing(data)
		if perr != nil {
			s.closeWith(c, nc, websocket.StatusPolicyViolation, "malformed")
			return metrics.ResultProtocol, perr
		}
		if s.hooks.OnPing != nil {
			s.hooks.OnPing(ctx, seq)
		}
		buf = proto.AppendPong(buf[:0], seq)
		if err := s.writeFrame(ctx, c, nc, buf); err != nil {
			return s.writeFailed(err), err
		}
		pings++
		if !internal {
			s.m.Pings.Inc()
		}
	}
	s.closeWith(c, nc, websocket.StatusNormalClosure, "done")
	return metrics.ResultCompleted, nil
}

// readFrame reads one message. Every call carries a net.Conn read deadline
// and a context deadline; no read in this package is unbounded.
func (s *Server) readFrame(ctx context.Context, c *websocket.Conn, nc net.Conn, dl time.Time) (websocket.MessageType, []byte, error) {
	_ = nc.SetReadDeadline(dl)
	rctx, cancel := context.WithDeadline(ctx, dl.Add(failsafeSlack))
	defer cancel()
	return c.Read(rctx)
}

// writeFrame writes one text message under the write deadline.
func (s *Server) writeFrame(ctx context.Context, c *websocket.Conn, nc net.Conn, p []byte) error {
	dl := time.Now().Add(s.cfg.WriteTimeout)
	_ = nc.SetWriteDeadline(dl)
	wctx, cancel := context.WithDeadline(ctx, dl.Add(failsafeSlack))
	defer cancel()
	return c.Write(wctx, websocket.MessageText, p)
}

func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Server) readFailed(c *websocket.Conn, nc net.Conn, err error, lifetime bool) string {
	switch {
	case isTimeout(err) && lifetime:
		s.closeWith(c, nc, websocket.StatusNormalClosure, "session time limit")
		return metrics.ResultLifetime
	case isTimeout(err):
		s.m.ReadTimeouts.Inc()
		s.closeWith(c, nc, websocket.StatusGoingAway, "idle")
		return metrics.ResultIdle
	case errors.Is(err, websocket.ErrMessageTooBig):
		return metrics.ResultTooBig // the library already sent 1009
	case websocket.CloseStatus(err) != -1:
		return metrics.ResultClientClosed
	case s.base.Err() != nil:
		s.closeWith(c, nc, websocket.StatusGoingAway, "going away")
		return metrics.ResultShutdown
	default:
		s.errs.Add(s.now(), "read", err.Error())
		return metrics.ResultError
	}
}

func (s *Server) writeFailed(err error) string {
	switch {
	case isTimeout(err):
		s.m.WriteTimeouts.Inc()
		s.sum.Inc("write_timeout")
		return metrics.ResultWriteTimeout
	case s.base.Err() != nil:
		return metrics.ResultShutdown
	case errors.Is(err, net.ErrClosed), errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.ECONNRESET):
		return metrics.ResultClientClosed
	default:
		s.errs.Add(s.now(), "write", err.Error())
		return metrics.ResultError
	}
}

// closeWith runs the close handshake under a net deadline of CloseTimeout and
// then closes the socket regardless of the outcome. The library's own close
// wait is a fixed 5 s, so the deadline is what enforces the spec's 2 s.
func (s *Server) closeWith(c *websocket.Conn, nc net.Conn, code websocket.StatusCode, reason string) {
	_ = nc.SetDeadline(time.Now().Add(s.cfg.CloseTimeout))
	_ = c.Close(code, reason)
	_ = c.CloseNow()
}
