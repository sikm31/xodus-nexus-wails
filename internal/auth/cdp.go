package auth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

type CDPEvent struct {
	Method    string         `json:"method"`
	Params    map[string]any `json:"params"`
	SessionID string         `json:"sessionId,omitempty"`
}

type pendingCall struct {
	result chan json.RawMessage
	err    chan error
}

// CDP speaks Chrome's private fd3/fd4 protocol. There is no listening socket
// and no protocol logging.
type CDP struct {
	input   io.Writer
	event   func(CDPEvent)
	failed  func()
	timeout time.Duration

	mu      sync.Mutex
	next    int64
	pending map[int64]pendingCall
	stopped bool
}

func NewCDP(input io.Writer, output io.Reader, event func(CDPEvent), failed func(), timeout time.Duration) *CDP {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	c := &CDP{input: input, event: event, failed: failed, timeout: timeout, pending: map[int64]pendingCall{}}
	go c.read(output)
	return c
}

func splitNull(data []byte, atEOF bool) (int, []byte, error) {
	if index := bytes.IndexByte(data, 0); index >= 0 {
		return index + 1, data[:index], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("truncated message")
	}
	return 0, nil, nil
}

func (c *CDP) read(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	scanner.Split(splitNull)
	for scanner.Scan() {
		var value struct {
			ID     *float64        `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
			CDPEvent
		}
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			break
		}
		if value.ID != nil {
			c.mu.Lock()
			call, ok := c.pending[int64(*value.ID)]
			delete(c.pending, int64(*value.ID))
			c.mu.Unlock()
			if !ok {
				continue
			}
			if len(value.Error) > 0 && string(value.Error) != "null" {
				call.err <- errors.New("Browser sign-in command failed.")
			} else {
				call.result <- value.Result
			}
		} else if value.Method != "" && value.Params != nil {
			c.event(value.CDPEvent)
		}
	}
	c.Close()
}

// Call sends a command and decodes its result into out (which may be nil).
func (c *CDP) Call(method string, params any, sessionID string, out any) error {
	c.mu.Lock()
	if c.stopped || len(c.pending) >= 128 {
		c.mu.Unlock()
		return errors.New("Browser sign-in connection is unavailable.")
	}
	c.next++
	id := c.next
	call := pendingCall{result: make(chan json.RawMessage, 1), err: make(chan error, 1)}
	c.pending[id] = call
	if params == nil {
		params = map[string]any{}
	}
	message := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		message["sessionId"] = sessionID
	}
	data, _ := json.Marshal(message)
	_, err := c.input.Write(append(data, 0))
	c.mu.Unlock()
	if err != nil {
		c.Close()
		return errors.New("Browser sign-in connection closed.")
	}
	select {
	case result := <-call.result:
		if out != nil && len(result) > 0 {
			return json.Unmarshal(result, out)
		}
		return nil
	case err := <-call.err:
		return err
	case <-time.After(c.timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return errors.New("Browser sign-in did not respond.")
	}
}

func (c *CDP) Close() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	pending := c.pending
	c.pending = map[int64]pendingCall{}
	c.mu.Unlock()
	for _, call := range pending {
		call.err <- errors.New("Browser sign-in connection closed.")
	}
	c.failed()
}
