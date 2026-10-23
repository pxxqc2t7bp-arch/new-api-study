package smartrouterhost

import (
	"io"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

const subscriptionCaptureLimit = 16 << 10

// Observe only bytes the normal handler reads; Close retains upstream ownership.
type subscriptionBody struct {
	body      io.ReadCloser
	state     *State
	attempt   int
	mu        sync.Mutex
	capture   []byte
	reading   int
	eof       bool
	invalid   bool
	closed    bool
	finalized bool
}

func (b *subscriptionBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.reading++
	b.invalid = b.invalid || b.closed
	b.mu.Unlock()

	// Close must be able to unblock a provider Read.
	n, err := b.body.Read(p)

	b.mu.Lock()
	b.reading--
	if err != nil && err != io.EOF {
		b.invalid = true
	}
	if !b.invalid {
		if n > subscriptionCaptureLimit-len(b.capture) {
			b.invalid = true
		} else {
			b.capture = append(b.capture, p[:n]...)
		}
	}
	b.eof = b.eof || err == io.EOF
	if b.invalid {
		b.capture = nil
	}
	b.mu.Unlock()
	return n, err
}

func (b *subscriptionBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.invalid = b.invalid || !b.eof || b.reading != 0
	b.mu.Unlock()

	err := b.body.Close()

	b.mu.Lock()
	b.invalid = b.invalid || err != nil
	if b.finalized && !b.invalid {
		b.mu.Unlock()
		return err
	}
	b.finalized = true
	capture := b.capture
	b.capture = nil
	valid := !b.invalid
	b.mu.Unlock()

	expired := valid && subscriptionExpiredDiagnostic(capture)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.mu.Lock()
	if b.state.active && b.state.invoked && b.state.attempts == b.attempt &&
		!b.state.unknown && b.state.rawStatus == 400 {
		b.state.subscriptionExpired = expired && !b.invalid
	}
	b.state.mu.Unlock()
	return err
}

func subscriptionExpiredDiagnostic(body []byte) bool {
	var fields map[string]common.RawMessage
	if common.Unmarshal(body, &fields) != nil || fields == nil {
		return false
	}
	// Reuse the syntax-validated wire scanner to reject duplicate fields.
	scanner := jsonFieldScanner{data: body}
	if scanner.value() != nil {
		return false
	}
	var message string
	if raw, exists := fields["error"]; exists && common.GetJsonType(raw) == "object" {
		// Do not combine root discriminators/messages with an object envelope.
		for _, name := range []string{"code", "reason", "message"} {
			if _, exists := fields[name]; exists {
				return false
			}
		}
		if common.Unmarshal(raw, &fields) != nil || fields == nil {
			return false
		}
		if common.GetJsonType(fields["message"]) != "string" || common.Unmarshal(fields["message"], &message) != nil {
			return false
		}
	} else {
		raw, exists := fields["message"]
		if !exists {
			raw = fields["error"]
		}
		if common.GetJsonType(raw) != "string" || common.Unmarshal(raw, &message) != nil {
			return false
		}
		if other, exists := fields["error"]; exists {
			var errorMessage string
			if common.GetJsonType(other) != "string" || common.Unmarshal(other, &errorMessage) != nil ||
				errorMessage != message {
				return false
			}
		}
	}
	found := false
	for _, name := range []string{"code", "reason"} {
		if raw, exists := fields[name]; exists {
			var code string
			if common.GetJsonType(raw) != "string" || common.Unmarshal(raw, &code) != nil || code != "invalid_request" {
				return false
			}
			found = true
		}
	}
	if !found {
		return false
	}
	switch strings.ToLower(strings.Join(strings.Fields(message), " ")) {
	case "your agentplan subscription is invalid or expired.",
		"your agentplan subscription has expired.",
		"\u60a8\u7684 agentplan \u8ba2\u9605\u5df2\u5931\u6548\u3002",
		"agentplan \u5df2\u8fc7\u671f\uff0c\u8bf7\u7eed\u8ba2\u540e\u91cd\u8bd5\u3002":
		return true
	}
	return false
}
