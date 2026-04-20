package integration_tests

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/internal/integration_tests/mocks"
	"github.com/apernet/hysteria/core/v2/server"
)

type disconnectEvent struct {
	ID       string
	Addr     string
	Duration time.Duration
	Err      error
}

type recordingDisconnectLogger struct {
	events chan disconnectEvent
}

func (l *recordingDisconnectLogger) LogDisconnect(id string, addr net.Addr, duration time.Duration, err error) {
	l.events <- disconnectEvent{
		ID:       id,
		Addr:     addr.String(),
		Duration: duration,
		Err:      err,
	}
}

type closableDisconnectLogger struct {
	closed chan struct{}
}

func (l *closableDisconnectLogger) LogDisconnect(string, net.Addr, time.Duration, error) {}

func (l *closableDisconnectLogger) Close() error {
	close(l.closed)
	return nil
}

type orderedDisconnectLogger struct {
	logged        chan struct{}
	closed        chan struct{}
	logAfterClose atomic.Bool
	mu            sync.Mutex
	isClosed      bool
}

func (l *orderedDisconnectLogger) LogDisconnect(string, net.Addr, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isClosed {
		l.logAfterClose.Store(true)
	}
	select {
	case l.logged <- struct{}{}:
	default:
	}
}

func (l *orderedDisconnectLogger) Close() error {
	l.mu.Lock()
	l.isClosed = true
	l.mu.Unlock()

	select {
	case l.closed <- struct{}{}:
	default:
	}
	return nil
}

func TestClientServerDisconnectLogger(t *testing.T) {
	udpConn, udpAddr, err := serverConn()
	require.NoError(t, err)

	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")

	disconnectLogger := &recordingDisconnectLogger{
		events: make(chan disconnectEvent, 1),
	}

	s, err := server.NewServer(&server.Config{
		TLSConfig:        serverTLSConfig(),
		Conn:             udpConn,
		Authenticator:    auth,
		DisconnectLogger: disconnectLogger,
	})
	require.NoError(t, err)
	defer s.Close()
	go s.Serve()

	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, c.Close())

	select {
	case event := <-disconnectLogger.events:
		assert.Equal(t, "nobody", event.ID)
		assert.NotEmpty(t, event.Addr)
		assert.Greater(t, event.Duration, time.Duration(0))
		assert.NoError(t, event.Err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for disconnect event")
	}
}

func TestServerCloseClosesDisconnectLogger(t *testing.T) {
	udpConn, _, err := serverConn()
	require.NoError(t, err)

	auth := mocks.NewMockAuthenticator(t)
	logger := &closableDisconnectLogger{closed: make(chan struct{}, 1)}

	s, err := server.NewServer(&server.Config{
		TLSConfig:        serverTLSConfig(),
		Conn:             udpConn,
		Authenticator:    auth,
		DisconnectLogger: logger,
	})
	require.NoError(t, err)

	require.NoError(t, s.Close())

	select {
	case <-logger.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for disconnect logger close")
	}
}

func TestServerCloseWaitsForDisconnectLogging(t *testing.T) {
	udpConn, udpAddr, err := serverConn()
	require.NoError(t, err)

	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")

	logger := &orderedDisconnectLogger{
		logged: make(chan struct{}, 1),
		closed: make(chan struct{}, 1),
	}

	s, err := server.NewServer(&server.Config{
		TLSConfig:        serverTLSConfig(),
		Conn:             udpConn,
		Authenticator:    auth,
		DisconnectLogger: logger,
	})
	require.NoError(t, err)
	go s.Serve()

	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	require.NoError(t, err)
	defer c.Close()

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, s.Close())

	select {
	case <-logger.logged:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for disconnect event before close")
	}

	select {
	case <-logger.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for disconnect logger close")
	}

	assert.False(t, logger.logAfterClose.Load())
}
