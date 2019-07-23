package p2p

import (
	"github.com/oasislabs/ekiden/go/common/crypto/signature"
	"github.com/oasislabs/ekiden/go/roothash/api/commitment"
)

// NOTE: Bump CommitteeProtocol version in go/common/version if you
//       change any of the structures below.

// Message is a message sent to nodes via P2P transport.
type Message struct {
	_struct struct{} `codec:",omitempty"` // nolint

	// RuntimeID is the identifier of the runtime this message
	// belongs to. It is used as a namespace.
	RuntimeID signature.PublicKey `codec:"runtime_id"`

	// GroupVersion is the version of all elected committees (the consensus
	// block height of last processed committee election). Messages with
	// non-matching group versions will be discarded.
	GroupVersion int64 `codec:"group_version"`

	// Jaeger's span context in binary format.
	SpanContext []byte `codec:"span"`

	// Message types.

	Ack   *Ack
	Error *Error

	SignedTxnSchedulerBatchDispatch *commitment.SignedTxnSchedulerBatchDispatch
	ComputeWorkerFinished           *ComputeWorkerFinished
}

// ComputeWorkerFinished is the message sent from the compute workers to
// the merge committee after a batch has been processed and is ready to
// be merged.
type ComputeWorkerFinished struct {
	// Commitment is a compute worker commitment.
	Commitment commitment.ComputeCommitment
}

// Ack is an acknowledgement that a message was received.
type Ack struct {
}

// Error is an error response.
type Error struct {
	// Message is an error message.
	Message string
}
