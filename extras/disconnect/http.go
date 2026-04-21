package disconnect

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/apernet/hysteria/core/v2/server"
	"go.uber.org/zap"
)

const (
	defaultQueueSize        = 10240
	defaultHTTPTimeout      = 8 * time.Second
	defaultCloseTimeout     = 5 * time.Second
	dropOldestWarnMessage   = "disconnect event queue full, dropping oldest"
	closeTimeoutWarnMessage = "disconnect logger close timed out, dropping pending events"
)

var (
	_ server.DisconnectLogger = (*HTTPDisconnectLogger)(nil)

	defaultRetryDelays = []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}

	errInvalidStatusCode = errors.New("invalid status code")
)

type disconnectEvent struct {
	ID         string `json:"id"`
	Addr       string `json:"addr"`
	DurationMS int64  `json:"duration_ms"`
	Reason     string `json:"reason"`
	Category   string `json:"category"`
}

type httpDisconnectLoggerOptions struct {
	client           *http.Client
	queueSize        int
	retryDelays      []time.Duration
	closeTimeout     time.Duration
	sleep            func(time.Duration)
	beforeDropOldest func(*HTTPDisconnectLogger)
	disableWorker    bool
}

type HTTPDisconnectLogger struct {
	client           *http.Client
	logger           *zap.Logger
	url              string
	queue            []disconnectEvent
	queueSize        int
	queueHead        int
	queueLen         int
	retryDelays      []time.Duration
	closeTimeout     time.Duration
	sleep            func(time.Duration)
	beforeDropOldest func(*HTTPDisconnectLogger)
	done             chan struct{}
	closing          chan struct{}
	forceStop        chan struct{}
	workerCtx        context.Context
	cancelWorker     context.CancelFunc

	mu            sync.Mutex
	cond          *sync.Cond
	closed        bool
	forceStopped  bool
	closeOnce     sync.Once
	forceStopOnce sync.Once
}

func NewHTTPDisconnectLogger(url string, insecure bool, logger *zap.Logger) *HTTPDisconnectLogger {
	return newHTTPDisconnectLogger(url, insecure, logger, httpDisconnectLoggerOptions{})
}

func newHTTPDisconnectLogger(url string, insecure bool, logger *zap.Logger, opts httpDisconnectLoggerOptions) *HTTPDisconnectLogger {
	if logger == nil {
		logger = zap.NewNop()
	}

	queueSize := opts.queueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}

	retryDelays := opts.retryDelays
	if len(retryDelays) == 0 {
		retryDelays = defaultRetryDelays
	}

	closeTimeout := opts.closeTimeout
	if closeTimeout <= 0 {
		closeTimeout = defaultCloseTimeout
	}

	client := opts.client
	if client == nil {
		client = newHTTPClient(insecure)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	l := &HTTPDisconnectLogger{
		client:           client,
		logger:           logger,
		url:              url,
		queue:            make([]disconnectEvent, queueSize),
		queueSize:        queueSize,
		retryDelays:      retryDelays,
		closeTimeout:     closeTimeout,
		sleep:            opts.sleep,
		beforeDropOldest: opts.beforeDropOldest,
		done:             workerDone,
		closing:          make(chan struct{}),
		forceStop:        make(chan struct{}),
		workerCtx:        workerCtx,
		cancelWorker:     cancelWorker,
	}
	l.cond = sync.NewCond(&l.mu)

	if !opts.disableWorker {
		go l.run()
	} else {
		close(workerDone)
	}

	return l
}

func newHTTPClient(insecure bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: insecure,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   defaultHTTPTimeout,
	}
}

func (l *HTTPDisconnectLogger) LogDisconnect(id string, addr net.Addr, duration time.Duration, err error) {
	l.enqueue(disconnectEvent{
		ID:         id,
		Addr:       addr.String(),
		DurationMS: duration.Milliseconds(),
		Reason:     disconnectReason(err),
		Category:   classifyDisconnect(err),
	})
}

func (l *HTTPDisconnectLogger) enqueue(event disconnectEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed || l.forceStopped {
		return
	}

	if l.queueLen == l.queueSize {
		if l.beforeDropOldest != nil {
			l.beforeDropOldest(l)
		}
		if l.queueLen == l.queueSize {
			l.queue[l.queueHead] = disconnectEvent{}
			l.queueHead = (l.queueHead + 1) % l.queueSize
			l.queueLen--
			l.logger.Warn(dropOldestWarnMessage)
		}
	}

	tail := (l.queueHead + l.queueLen) % l.queueSize
	l.queue[tail] = event
	l.queueLen++
	l.cond.Signal()
}

func (l *HTTPDisconnectLogger) run() {
	defer close(l.done)

	for {
		event, ok := l.nextEvent()
		if !ok {
			return
		}
		l.sendWithRetry(event)
	}
}

func (l *HTTPDisconnectLogger) sendWithRetry(event disconnectEvent) {
	for attempt := 0; ; attempt++ {
		if l.isForceStopped() {
			return
		}
		err := l.post(event)
		if err == nil {
			return
		}
		if l.isForceStopped() {
			return
		}
		if l.isClosing() || attempt >= len(l.retryDelays) {
			l.logger.Warn("failed to deliver disconnect event",
				zap.String("id", event.ID),
				zap.String("addr", event.Addr),
				zap.Error(err))
			return
		}
		if !l.wait(l.retryDelays[attempt]) {
			if l.isForceStopped() {
				return
			}
			l.logger.Warn("failed to deliver disconnect event",
				zap.String("id", event.ID),
				zap.String("addr", event.Addr),
				zap.Error(err))
			return
		}
	}
}

func (l *HTTPDisconnectLogger) post(event disconnectEvent) error {
	if l.isForceStopped() {
		return context.Canceled
	}

	bs, err := json.Marshal(event)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(l.workerCtx, http.MethodPost, l.url, bytes.NewReader(bs))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return errInvalidStatusCode
	}
	return nil
}

func (l *HTTPDisconnectLogger) wait(delay time.Duration) bool {
	if l.sleep != nil {
		l.sleep(delay)
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-l.closing:
		return false
	case <-l.forceStop:
		return false
	}
}

func (l *HTTPDisconnectLogger) isClosing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *HTTPDisconnectLogger) isForceStopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.forceStopped
}

func (l *HTTPDisconnectLogger) nextEvent() (disconnectEvent, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for {
		if l.forceStopped {
			return disconnectEvent{}, false
		}
		if l.queueLen > 0 {
			event := l.queue[l.queueHead]
			l.queue[l.queueHead] = disconnectEvent{}
			l.queueHead = (l.queueHead + 1) % l.queueSize
			l.queueLen--
			return event, true
		}
		if l.closed {
			return disconnectEvent{}, false
		}
		l.cond.Wait()
	}
}

func (l *HTTPDisconnectLogger) forceStopNow() {
	l.forceStopOnce.Do(func() {
		l.mu.Lock()
		l.forceStopped = true
		l.queueHead = 0
		l.queueLen = 0
		close(l.forceStop)
		l.cond.Broadcast()
		l.mu.Unlock()

		l.cancelWorker()
	})
}

func (l *HTTPDisconnectLogger) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.closing)
		l.cond.Broadcast()
		l.mu.Unlock()

		timer := time.NewTimer(l.closeTimeout)
		defer timer.Stop()

		select {
		case <-l.done:
		case <-timer.C:
			l.logger.Warn(closeTimeoutWarnMessage)
			l.forceStopNow()
			<-l.done
		}
	})
	return nil
}

func disconnectReason(err error) string {
	if err == nil {
		return "closed"
	}
	return err.Error()
}
