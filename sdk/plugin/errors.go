package sdk

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Failures a plugin has to branch on.
//
// Core answers with gRPC status codes, which is right for the wire and wrong
// for the plugin author: testing for one meant importing google.golang.org/grpc
// and comparing codes, in a library whose whole purpose is that a plugin writes
// against the SDK rather than against the transport. The documentation said
// "returns FailedPrecondition, re-read and retry" and left the reader to work
// out how.
//
// These are the ones where getting it wrong is silent or expensive. Everything
// else stays an ordinary error with a message.
var (
	// ErrVersionConflict means someone else wrote the document first. The
	// caller's copy is stale: re-read, re-apply, try again.
	//
	// Inside a transaction as much as outside it. A transaction makes the
	// steps atomic, not uncontended — two transactions can both read a row,
	// and the second write is refused rather than allowed to overwrite. A
	// handler that treats this as a failure turns a contended document into a
	// wall of 500s, which is what the inventory example did until it retried.
	ErrVersionConflict = errors.New("version conflict")

	// ErrTxExpired means the transaction is gone — committed, rolled back, or
	// past its timeout. Retrying the write will never work; the whole
	// transaction has to be started again.
	//
	// Distinct from ErrVersionConflict on purpose. Both used to arrive as
	// FailedPrecondition, so a retry loop could not tell "try that write
	// again" from "this transaction no longer exists" and would spin.
	ErrTxExpired = errors.New("transaction expired")

	// ErrNotAllowed is a refusal that will not change by retrying: a
	// capability the manifest does not declare, or an outbound host outside
	// egress_allow. Fixing it means editing the manifest and having it
	// approved.
	ErrNotAllowed = errors.New("not allowed")

	// ErrRateLimited is a ceiling that clears on its own — the outbound
	// request rate, or a full queue. Back off and retry.
	ErrRateLimited = errors.New("rate limited")

	// ErrNotFound is a document, file or message that is not there.
	ErrNotFound = errors.New("not found")

	// ErrHostUnavailable means the host capabilities are not bound yet: this
	// code ran outside a live Core, which in practice means it ran under the
	// plugin's own `go test`.
	//
	// It exists so that reaching for a capability in a test produces a sentence
	// rather than a segmentation fault. Building a query still works — see
	// Query.Describe, which is how query-construction logic is tested — and only
	// executing one fails.
	ErrHostUnavailable = errors.New("host capabilities are not available outside a running Core")
)

// hostErr attaches a sentinel to an error from Core so callers can use
// errors.Is on it. The original message is preserved: the sentinel says what
// kind of failure it is, the message says which one.
func hostErr(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	var kind error
	switch st.Code() {
	case codes.Aborted:
		kind = ErrVersionConflict
	case codes.FailedPrecondition:
		kind = ErrTxExpired
	case codes.PermissionDenied:
		kind = ErrNotAllowed
	case codes.ResourceExhausted:
		kind = ErrRateLimited
	case codes.NotFound:
		kind = ErrNotFound
	default:
		return err
	}
	return &hostError{kind: kind, err: err}
}

type hostError struct {
	kind error
	err  error
}

func (e *hostError) Error() string { return e.err.Error() }

// Is matches both the sentinel and anything the underlying error matches, so
// wrapping does not hide what Core sent.
func (e *hostError) Is(target error) bool { return target == e.kind }

func (e *hostError) Unwrap() error { return e.err }
