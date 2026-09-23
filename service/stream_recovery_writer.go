package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

type StreamRecoveryWriter struct {
	store           *StreamRecoveryStore
	ctx             context.Context
	streamID        string
	attempt         int
	header          http.Header
	status          int
	size            int
	businessSize    int
	sequence        int64
	buffer          []byte
	terminal        bool
	failed          bool
	attemptError    error
	writeError      error
	commitUncertain bool
	onFrame         func(attempt int, sequence int64) error
	mutex           sync.Mutex
}

var _ gin.ResponseWriter = (*StreamRecoveryWriter)(nil)

var (
	ErrStreamRecoveryTerminal        = errors.New("stream recovery stream is already terminal")
	ErrStreamRecoveryFrameRejected   = errors.New("stream recovery frame commit rejected")
	ErrStreamRecoveryCommitUncertain = errors.New("stream recovery frame commit is uncertain")
)

func NewStreamRecoveryWriter(
	ctx context.Context,
	store *StreamRecoveryStore,
	streamID string,
	attempt int,
	onFrame func(attempt int, sequence int64) error,
) *StreamRecoveryWriter {
	if ctx == nil {
		ctx = context.Background()
	}
	return &StreamRecoveryWriter{
		store:    store,
		ctx:      ctx,
		streamID: streamID,
		attempt:  attempt,
		header:   make(http.Header),
		onFrame:  onFrame,
	}
}

func (writer *StreamRecoveryWriter) Header() http.Header {
	return writer.header
}

func (writer *StreamRecoveryWriter) Write(data []byte) (int, error) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.writeError != nil {
		return 0, writer.writeError
	}
	if writer.attemptError != nil {
		return 0, writer.attemptError
	}
	if writer.terminal {
		return 0, ErrStreamRecoveryTerminal
	}
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	writer.buffer = append(writer.buffer, data...)
	writer.size += len(data)
	return len(data), nil
}

func (writer *StreamRecoveryWriter) WriteString(data string) (int, error) {
	return writer.Write([]byte(data))
}

func (writer *StreamRecoveryWriter) WriteHeader(statusCode int) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.status == 0 {
		writer.status = statusCode
	}
}

func (writer *StreamRecoveryWriter) WriteHeaderNow() {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
}

func (writer *StreamRecoveryWriter) Status() int {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.status
}

func (writer *StreamRecoveryWriter) Size() int {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.size
}

func (writer *StreamRecoveryWriter) Written() bool {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.status != 0 || writer.size > 0
}

func (writer *StreamRecoveryWriter) HasBusinessOutput() bool {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.commitUncertain ||
		writer.businessSize > 0 ||
		StreamRecoveryFrameHasBusinessData(writer.buffer)
}

func (writer *StreamRecoveryWriter) Flush() {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	writer.flushLocked()
}

func (writer *StreamRecoveryWriter) FlushError() error {
	writer.Flush()
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.writeError != nil {
		return writer.writeError
	}
	if writer.attemptError != nil {
		return writer.attemptError
	}
	return writer.writeError
}

func (writer *StreamRecoveryWriter) CloseNotify() <-chan bool {
	channel := make(chan bool)
	return channel
}

func (writer *StreamRecoveryWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("stream recovery writer does not support hijacking")
}

func (writer *StreamRecoveryWriter) Pusher() http.Pusher {
	return nil
}

func (writer *StreamRecoveryWriter) Attempt() int {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.attempt
}

func (writer *StreamRecoveryWriter) StreamID() string {
	return writer.streamID
}

func (writer *StreamRecoveryWriter) Sequence() int64 {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.sequence
}

func (writer *StreamRecoveryWriter) Terminal() bool {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.terminal
}

func (writer *StreamRecoveryWriter) FailedTerminal() bool {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.failed
}

func (writer *StreamRecoveryWriter) MarkAttemptState(status string, errorMessage string) error {
	writer.mutex.Lock()
	attempt := writer.attempt
	sequence := writer.sequence
	writer.mutex.Unlock()
	return writer.store.SetAttemptState(
		writer.ctx,
		writer.streamID,
		attempt,
		StreamRecoveryAttemptState{
			Status:       status,
			LastSequence: sequence,
			Error:        errorMessage,
		},
	)
}

func (writer *StreamRecoveryWriter) RotateAttempt(attempt int) error {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if writer.writeError != nil {
		return writer.writeError
	}
	if attempt <= writer.attempt {
		return fmt.Errorf("stream recovery attempt must increase: current=%d next=%d", writer.attempt, attempt)
	}
	writer.attempt = attempt
	writer.status = 0
	writer.size = 0
	writer.businessSize = 0
	writer.sequence = 0
	writer.buffer = nil
	writer.terminal = false
	writer.failed = false
	writer.attemptError = nil
	writer.commitUncertain = false
	writer.header = make(http.Header)
	return nil
}

func (writer *StreamRecoveryWriter) PublishAttempt() error {
	writer.mutex.Lock()
	attempt := writer.attempt
	writer.mutex.Unlock()
	return writer.store.SetPublicAttempt(writer.ctx, writer.streamID, attempt)
}

func (writer *StreamRecoveryWriter) flushLocked() {
	if writer.writeError != nil || writer.attemptError != nil || len(writer.buffer) == 0 {
		return
	}
	for _, frame := range splitStreamRecoveryFlush(writer.buffer) {
		sequence := writer.sequence + 1
		data := appendStreamRecoveryEventID(frame, writer.attempt, sequence)
		terminal, failed := StreamRecoveryTerminalState(data)
		storedFrame := StreamRecoveryFrame{
			Sequence: sequence,
			Kind:     "data",
			Data:     data,
			Terminal: terminal,
		}
		storedFrame, err := writer.store.AppendFrameForRollback(
			writer.ctx,
			writer.streamID,
			writer.attempt,
			storedFrame,
		)
		if err != nil {
			writer.writeError = err
			return
		}
		if writer.onFrame != nil {
			if err := writer.onFrame(writer.attempt, sequence); err != nil {
				if !errors.Is(err, ErrStreamRecoveryFrameRejected) {
					writer.buffer = writer.buffer[:0]
					writer.commitUncertain = true
					writer.writeError = err
					return
				}
				rollbackErr := writer.store.RollbackFrame(
					writer.ctx,
					writer.streamID,
					writer.attempt,
					storedFrame,
				)
				writer.buffer = writer.buffer[:0]
				if rollbackErr != nil {
					writer.writeError = fmt.Errorf(
						"%w; stream recovery frame rollback failed: %v",
						err,
						rollbackErr,
					)
				} else {
					writer.attemptError = err
				}
				return
			}
		}
		writer.sequence = sequence
		if StreamRecoveryFrameHasBusinessData(data) {
			writer.businessSize += len(data)
		}
		writer.terminal = writer.terminal || terminal
		writer.failed = writer.failed || failed
		if terminal {
			break
		}
	}
	writer.buffer = writer.buffer[:0]
}

func StreamRecoveryFrameHasBusinessData(data []byte) bool {
	if terminal, failed := StreamRecoveryTerminalState(data); terminal && failed {
		return false
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			return true
		}
		switch field {
		case "id", "retry":
			continue
		case "event":
			if strings.TrimSpace(value) != "" {
				return true
			}
		case "data":
			return true
		default:
			return true
		}
	}
	return false
}

func splitStreamRecoveryFlush(data []byte) [][]byte {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	var frames [][]byte
	for {
		end := strings.Index(normalized, "\n\n")
		if end < 0 {
			if normalized != "" {
				frames = append(frames, []byte(normalized))
			}
			return frames
		}
		end += 2
		frames = append(frames, []byte(normalized[:end]))
		normalized = normalized[end:]
	}
}

func appendStreamRecoveryEventID(frame []byte, attempt int, sequence int64) []byte {
	if len(frame) == 0 || !looksLikeSSEFrame(frame) {
		return append([]byte(nil), frame...)
	}
	lines := strings.Split(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\n")
	lines = slices.DeleteFunc(lines, func(line string) bool {
		field, _, _ := strings.Cut(line, ":")
		return field == "id"
	})
	return fmt.Appendf(
		nil,
		"id: %d:%d\n%s",
		attempt,
		sequence,
		strings.Join(lines, "\n"),
	)
}

func looksLikeSSEFrame(frame []byte) bool {
	for _, line := range strings.Split(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\n") {
		field, _, _ := strings.Cut(line, ":")
		if field == "data" || field == "event" {
			return true
		}
	}
	return false
}

func StreamRecoveryTerminalState(frame []byte) (terminal bool, failed bool) {
	normalized := strings.ReplaceAll(string(frame), "\r\n", "\n")
	for _, block := range strings.Split(normalized, "\n\n") {
		var event string
		var data []string
		for _, line := range strings.Split(block, "\n") {
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				data = append(data, value)
			}
		}
		if event == "error" ||
			event == "response.error" ||
			event == "response.failed" ||
			event == "response.incomplete" ||
			event == "response.cancelled" ||
			event == "response.canceled" {
			return true, true
		}
		if event == "response.completed" ||
			event == "response.done" ||
			event == "message_stop" ||
			strings.Join(data, "\n") == "[DONE]" {
			return true, false
		}
	}
	return false, false
}
