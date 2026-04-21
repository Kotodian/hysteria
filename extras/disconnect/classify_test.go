package disconnect

import (
	"errors"
	"fmt"
	"testing"

	"github.com/apernet/quic-go"
)

func TestClassifyDisconnect(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error is clean close",
			err:  nil,
			want: categoryClosed,
		},
		{
			name: "client-initiated application close",
			err: &quic.ApplicationError{
				Remote:    true,
				ErrorCode: 0,
			},
			want: categoryClientClose,
		},
		{
			name: "server-initiated application close",
			err: &quic.ApplicationError{
				Remote:    false,
				ErrorCode: 0,
			},
			want: categoryServerClose,
		},
		{
			name: "idle timeout",
			err:  &quic.IdleTimeoutError{},
			want: categoryIdleTimeout,
		},
		{
			name: "handshake timeout",
			err:  &quic.HandshakeTimeoutError{},
			want: categoryHandshakeTimeout,
		},
		{
			name: "stateless reset",
			err:  &quic.StatelessResetError{},
			want: categoryStatelessReset,
		},
		{
			name: "version negotiation",
			err:  &quic.VersionNegotiationError{},
			want: categoryVersionNegotiation,
		},
		{
			name: "client-initiated transport error",
			err: &quic.TransportError{
				Remote:    true,
				ErrorCode: quic.InternalError,
			},
			want: categoryClientTransportError,
		},
		{
			name: "server-initiated transport error",
			err: &quic.TransportError{
				Remote:    false,
				ErrorCode: quic.InternalError,
			},
			want: categoryServerTransportError,
		},
		{
			name: "wrapped client close is still classified",
			err: fmt.Errorf("handle conn: %w", &quic.ApplicationError{
				Remote:    true,
				ErrorCode: 42,
			}),
			want: categoryClientClose,
		},
		{
			name: "plain error falls through to unknown",
			err:  errors.New("something else"),
			want: categoryUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDisconnect(tt.err); got != tt.want {
				t.Errorf("classifyDisconnect() = %q, want %q", got, tt.want)
			}
		})
	}
}
