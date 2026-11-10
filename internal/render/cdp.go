package render

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// conn speaks the Chrome DevTools Protocol over --remote-debugging-pipe:
// JSON messages, each ended by a NUL byte, written to the browser's fd 3 and
// read from its fd 4. A pipe needs no network port and no WebSocket.
type conn struct {
	w      io.Writer
	wmu    sync.Mutex
	next   atomic.Int64
	pmu    sync.Mutex
	wait   map[int64]chan message
	events func(message) // called on the reading goroutine; must not block
	done   chan struct{}
	err    error
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newConn(w io.Writer, r io.Reader, events func(message)) *conn {
	c := &conn{w: w, wait: map[int64]chan message{}, events: events, done: make(chan struct{})}
	go c.read(r)
	return c
}

func (c *conn) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	var err error
	for {
		var b []byte
		b, err = br.ReadBytes(0)
		if err != nil {
			break
		}
		var m message
		if json.Unmarshal(bytes.TrimSuffix(b, []byte{0}), &m) != nil {
			continue
		}
		if m.ID == 0 {
			c.events(m)
			continue
		}
		c.pmu.Lock()
		ch := c.wait[m.ID]
		delete(c.wait, m.ID)
		c.pmu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	c.err = err
	close(c.done)
}

// call sends one command and waits for its answer. session is "" for the
// browser, or the session of an attached page.
func (c *conn) call(ctx context.Context, session, method string, params, out any) error {
	id := c.next.Add(1)
	ch := make(chan message, 1)
	c.pmu.Lock()
	c.wait[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.wait, id)
		c.pmu.Unlock()
	}()
	if params == nil {
		params = struct{}{}
	}
	b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params, "sessionId": session})
	if err != nil {
		return err
	}
	if session == "" {
		b, _ = json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	}
	c.wmu.Lock()
	_, err = c.w.Write(append(b, 0))
	c.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		return fmt.Errorf("%s: the browser closed (%v)", method, c.err)
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	}
}
