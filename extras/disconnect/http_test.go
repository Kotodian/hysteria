package disconnect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func httpResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(http.NoBody),
		Header:     make(http.Header),
	}
}

func queuedIDs(logger *HTTPDisconnectLogger) []string {
	logger.mu.Lock()
	defer logger.mu.Unlock()

	ids := make([]string, logger.queueLen)
	for i := 0; i < logger.queueLen; i++ {
		event := logger.queue[(logger.queueHead+i)%logger.queueSize]
		ids[i] = event.ID
	}
	return ids
}

func TestHTTPDisconnectLogger_POSTPayload(t *testing.T) {
	type payload struct {
		ID         string `json:"id"`
		Addr       string `json:"addr"`
		DurationMS int64  `json:"duration_ms"`
		Reason     string `json:"reason"`
		Category   string `json:"category"`
	}

	payloadCh := make(chan payload, 1)
	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var got payload
			err := json.NewDecoder(r.Body).Decode(&got)
			require.NoError(t, err)

			payloadCh <- got
			return httpResponse(http.StatusOK), nil
		}),
	}

	logger := newHTTPDisconnectLogger("http://disconnect.example", false, zap.NewNop(), httpDisconnectLoggerOptions{
		client: client,
	})
	t.Cleanup(func() {
		require.NoError(t, logger.Close())
	})

	logger.LogDisconnect("user-1", &net.TCPAddr{
		IP:   net.IPv4(1, 2, 3, 4),
		Port: 5678,
	}, 12345*time.Millisecond, errors.New("remote reset"))

	select {
	case got := <-payloadCh:
		assert.Equal(t, "user-1", got.ID)
		assert.Equal(t, "1.2.3.4:5678", got.Addr)
		assert.EqualValues(t, 12345, got.DurationMS)
		assert.Equal(t, "remote reset", got.Reason)
		assert.Equal(t, categoryUnknown, got.Category)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for disconnect payload")
	}
}

func TestHTTPDisconnectLogger_ClassifiesClientClose(t *testing.T) {
	type payload struct {
		Reason   string `json:"reason"`
		Category string `json:"category"`
	}

	payloadCh := make(chan payload, 1)
	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var got payload
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			payloadCh <- got
			return httpResponse(http.StatusOK), nil
		}),
	}

	logger := newHTTPDisconnectLogger("http://disconnect.example", false, zap.NewNop(), httpDisconnectLoggerOptions{
		client: client,
	})
	t.Cleanup(func() {
		require.NoError(t, logger.Close())
	})

	logger.LogDisconnect("user-2", &net.TCPAddr{
		IP:   net.IPv4(10, 0, 0, 1),
		Port: 4000,
	}, time.Second, &quic.ApplicationError{Remote: true, ErrorCode: 0})

	select {
	case got := <-payloadCh:
		assert.Equal(t, categoryClientClose, got.Category)
		assert.NotEmpty(t, got.Reason)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for disconnect payload")
	}
}

func TestHTTPDisconnectLogger_QueueDropsOldest(t *testing.T) {
	core, recorded := observer.New(zap.WarnLevel)
	logger := newHTTPDisconnectLogger("http://127.0.0.1:1", false, zap.New(core), httpDisconnectLoggerOptions{
		queueSize:     2,
		disableWorker: true,
	})

	addr := &net.TCPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 9000}
	logger.LogDisconnect("first", addr, time.Second, errors.New("first"))
	logger.LogDisconnect("second", addr, time.Second, errors.New("second"))
	logger.LogDisconnect("third", addr, time.Second, errors.New("third"))

	assert.Equal(t, []string{"second", "third"}, queuedIDs(logger))
	assert.Len(t, recorded.FilterMessage(dropOldestWarnMessage).All(), 1)

	require.NoError(t, logger.Close())
}

func TestHTTPDisconnectLogger_ExponentialBackoff(t *testing.T) {
	var attempts atomic.Int32
	var sleepMu sync.Mutex
	var sleeps []time.Duration
	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			attempts.Add(1)
			return httpResponse(http.StatusBadGateway), nil
		}),
	}

	logger := newHTTPDisconnectLogger("http://disconnect.example", false, zap.NewNop(), httpDisconnectLoggerOptions{
		client:      client,
		queueSize:   1,
		retryDelays: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second},
		sleep: func(d time.Duration) {
			sleepMu.Lock()
			sleeps = append(sleeps, d)
			sleepMu.Unlock()
		},
	})
	t.Cleanup(func() {
		require.NoError(t, logger.Close())
	})

	logger.LogDisconnect("user-2", &net.TCPAddr{
		IP:   net.IPv4(9, 9, 9, 9),
		Port: 443,
	}, time.Second, errors.New("backend unavailable"))

	require.Eventually(t, func() bool {
		return attempts.Load() == 6
	}, 2*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		sleepMu.Lock()
		defer sleepMu.Unlock()
		return len(sleeps) == 5
	}, 2*time.Second, 10*time.Millisecond)

	sleepMu.Lock()
	assert.Equal(t, []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}, sleeps)
	sleepMu.Unlock()
}

func TestHTTPDisconnectLogger_CloseFlushes(t *testing.T) {
	received := make(chan struct{}, 1)
	client := &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			received <- struct{}{}
			return httpResponse(http.StatusOK), nil
		}),
	}

	logger := newHTTPDisconnectLogger("http://disconnect.example", false, zap.NewNop(), httpDisconnectLoggerOptions{
		client: client,
	})
	logger.LogDisconnect("user-3", &net.TCPAddr{
		IP:   net.IPv4(10, 0, 0, 1),
		Port: 1443,
	}, 250*time.Millisecond, nil)

	require.NoError(t, logger.Close())

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for close flush")
	}
}

func TestHTTPDisconnectLogger_Accepts2xxResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "accepted", status: http.StatusAccepted},
		{name: "no content", status: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := newHTTPDisconnectLogger("http://disconnect.example", false, zap.NewNop(), httpDisconnectLoggerOptions{
				client: &http.Client{
					Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						return httpResponse(tt.status), nil
					}),
				},
				disableWorker: true,
			})

			err := logger.post(disconnectEvent{
				ID:         "user-4",
				Addr:       "127.0.0.1:443",
				DurationMS: 1000,
				Reason:     "closed",
			})

			assert.NoError(t, err)
			require.NoError(t, logger.Close())
		})
	}
}

func TestHTTPDisconnectLogger_DoesNotDropExtraEventWhenSpaceOpensBeforeDrop(t *testing.T) {
	core, recorded := observer.New(zap.WarnLevel)
	var consumedID string

	logger := newHTTPDisconnectLogger("http://127.0.0.1:1", false, zap.New(core), httpDisconnectLoggerOptions{
		queueSize:     2,
		disableWorker: true,
		beforeDropOldest: func(logger *HTTPDisconnectLogger) {
			consumedID = logger.queue[logger.queueHead].ID
			logger.queue[logger.queueHead] = disconnectEvent{}
			logger.queueHead = (logger.queueHead + 1) % logger.queueSize
			logger.queueLen--
		},
	})

	addr := &net.TCPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 9000}
	logger.LogDisconnect("first", addr, time.Second, errors.New("first"))
	logger.LogDisconnect("second", addr, time.Second, errors.New("second"))
	logger.LogDisconnect("third", addr, time.Second, errors.New("third"))

	assert.Equal(t, "first", consumedID)
	assert.Equal(t, []string{"second", "third"}, queuedIDs(logger))
	assert.Empty(t, recorded.FilterMessage(dropOldestWarnMessage).All())

	require.NoError(t, logger.Close())
}

func TestHTTPDisconnectLogger_CloseTimeoutStopsBackgroundDelivery(t *testing.T) {
	core, recorded := observer.New(zap.WarnLevel)
	started := make(chan struct{}, 2)
	requestDone := make(chan error, 2)
	release := make(chan struct{})

	logger := newHTTPDisconnectLogger("http://disconnect.example", false, zap.New(core), httpDisconnectLoggerOptions{
		client: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				started <- struct{}{}

				select {
				case <-r.Context().Done():
					err := r.Context().Err()
					requestDone <- err
					return nil, err
				case <-release:
					requestDone <- nil
					return httpResponse(http.StatusOK), nil
				}
			}),
		},
		queueSize:    2,
		closeTimeout: 20 * time.Millisecond,
	})

	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 443}
	logger.LogDisconnect("first", addr, time.Second, errors.New("first"))
	logger.LogDisconnect("second", addr, time.Second, errors.New("second"))

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first request to start")
	}

	require.NoError(t, logger.Close())
	close(release)

	select {
	case err := <-requestDone:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked request to stop")
	}

	select {
	case <-started:
		t.Fatal("unexpected request started after close timeout")
	case <-time.After(200 * time.Millisecond):
	}

	assert.Len(t, recorded.FilterMessage(closeTimeoutWarnMessage).All(), 1)
}
