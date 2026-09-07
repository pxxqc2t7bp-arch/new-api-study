package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

type StreamRecoveryWriter struct {
	store      *StreamRecoveryStore
	ctx        context.Context
	streamID   string
	attempt    int
	header     http.Header
	status     int
	size       int
	sequence   int64
	buffer     []byte
	terminal   bool
	writeError error
	onFrame    func(sequence int64) error
	mutex      sync.Mutex
}

var _ gin.ResponseWriter = (*StreamRecoveryWriter)(nil)

func NewStreamRecoveryWriter(
	ctx context.Context,
	store *StreamRecoveryStore,
	streamID string,
	attempt int,
	onFrame func(sequence int64) error,
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

func (writer *StreamRecoveryWriter) Flush() {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	writer.flushLocked()
}

func (writer *StreamRecoveryWriter) FlushError() error {
	writer.Flush()
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
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
	writer.sequence = 0
	writer.buffer = nil
	writer.terminal = false
	writer.header = make(http.Header)
	return writer.store.SetPublicAttempt(writer.ctx, writer.streamID, attempt)
}

func (writer *StreamRecoveryWriter) flushLocked() {
	if writer.writeError != nil || len(writer.buffer) == 0 {
		return
	}
	writer.sequence++
	data := appendStreamRecoveryEventID(writer.buffer, writer.sequence)
	terminal := isTerminalStreamRecoveryFrame(data)
	err := writer.store.AppendFrame(writer.ctx, writer.streamID, writer.attempt, StreamRecoveryFrame{
		Sequence: writer.sequence,
		Kind:     "data",
		Data:     data,
		Terminal: terminal,
	})
	if err == nil && writer.onFrame != nil {
		err = writer.onFrame(writer.sequence)
	}
	if err != nil {
		writer.writeError = err
		return
	}
	writer.terminal = writer.terminal || terminal
	writer.buffer = writer.buffer[:0]
}

func appendStreamRecoveryEventID(frame []byte, sequence int64) []byte {
	if len(frame) == 0 || frame[0] == ':' || !looksLikeSSEFrame(frame) {
		return append([]byte(nil), frame...)
	}
	prefix := []byte("id: " + strconv.FormatInt(sequence, 10) + "\n")
	result := make([]byte, 0, len(prefix)+len(frame))
	result = append(result, prefix...)
	result = append(result, frame...)
	return result
}

func looksLikeSSEFrame(frame []byte) bool {
	text := strings.TrimSpace(string(frame))
	return strings.HasPrefix(text, "data:") || strings.HasPrefix(text, "event:")
}

func isTerminalStreamRecoveryFrame(frame []byte) bool {
	text := string(frame)
	return strings.Contains(text, "event: response.completed") ||
		strings.Contains(text, "event: message_stop") ||
		strings.Contains(text, "data: [DONE]")
}
