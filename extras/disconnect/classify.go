package disconnect

import (
	"errors"

	"github.com/apernet/quic-go"
)

const (
	categoryClosed               = "closed"
	categoryClientClose          = "client_close"
	categoryServerClose          = "server_close"
	categoryIdleTimeout          = "idle_timeout"
	categoryHandshakeTimeout     = "handshake_timeout"
	categoryStatelessReset       = "stateless_reset"
	categoryVersionNegotiation   = "version_negotiation"
	categoryClientTransportError = "client_transport_error"
	categoryServerTransportError = "server_transport_error"
	categoryUnknown              = "unknown"
)

// classifyDisconnect inspects the error returned by quic-go when a QUIC
// connection ends and maps it to a stable category string suitable for
// webhook consumers. Remote-initiated close and local-initiated close are
// distinguished via the .Remote field that quic-go sets when it parses the
// CONNECTION_CLOSE frame.
func classifyDisconnect(err error) string {
	if err == nil {
		return categoryClosed
	}

	var appErr *quic.ApplicationError
	if errors.As(err, &appErr) {
		if appErr.Remote {
			return categoryClientClose
		}
		return categoryServerClose
	}

	var idleErr *quic.IdleTimeoutError
	if errors.As(err, &idleErr) {
		return categoryIdleTimeout
	}

	var hsErr *quic.HandshakeTimeoutError
	if errors.As(err, &hsErr) {
		return categoryHandshakeTimeout
	}

	var srErr *quic.StatelessResetError
	if errors.As(err, &srErr) {
		return categoryStatelessReset
	}

	var vnErr *quic.VersionNegotiationError
	if errors.As(err, &vnErr) {
		return categoryVersionNegotiation
	}

	var trErr *quic.TransportError
	if errors.As(err, &trErr) {
		if trErr.Remote {
			return categoryClientTransportError
		}
		return categoryServerTransportError
	}

	return categoryUnknown
}
