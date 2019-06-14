package committee

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/oasislabs/ekiden/go/common/crash"
	"github.com/oasislabs/ekiden/go/common/crypto/hash"
	"github.com/oasislabs/ekiden/go/common/logging"
	"github.com/oasislabs/ekiden/go/common/pubsub"
	"github.com/oasislabs/ekiden/go/common/runtime"
	"github.com/oasislabs/ekiden/go/common/tracing"
	roothash "github.com/oasislabs/ekiden/go/roothash/api"
	"github.com/oasislabs/ekiden/go/roothash/api/block"
	"github.com/oasislabs/ekiden/go/roothash/api/commitment"
	storage "github.com/oasislabs/ekiden/go/storage/api"
	"github.com/oasislabs/ekiden/go/worker/common/committee"
	"github.com/oasislabs/ekiden/go/worker/common/host"
	"github.com/oasislabs/ekiden/go/worker/common/host/protocol"
	"github.com/oasislabs/ekiden/go/worker/common/p2p"
	mergeCommittee "github.com/oasislabs/ekiden/go/worker/merge/committee"
)

var (
	errSeenNewerBlock    = errors.New("compute: seen newer block")
	errWorkerAborted     = errors.New("compute: worker aborted batch processing")
	errIncomatibleHeader = errors.New("compute: incompatible header")
	errIncorrectRole     = errors.New("compute: incorrect role")
	errIncorrectState    = errors.New("compute: incorrect state")
)

var (
	discrepancyDetectedCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ekiden_worker_compute_discrepancy_detected_count",
			Help: "Number of detected compute discrepancies",
		},
		[]string{"runtime"},
	)
	abortedBatchCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ekiden_worker_aborted_batch_count",
			Help: "Number of aborted batches",
		},
		[]string{"runtime"},
	)
	storageCommitLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "ekiden_worker_storage_commit_latency",
			Help: "Latency of storage commit calls (state + outputs)",
		},
		[]string{"runtime"},
	)
	batchProcessingTime = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "ekiden_worker_batch_processing_time",
			Help: "Time it takes for a batch to finalize",
		},
		[]string{"runtime"},
	)
	batchRuntimeProcessingTime = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "ekiden_worker_batch_runtime_processing_time",
			Help: "Time it takes for a batch to be processed by the runtime",
		},
		[]string{"runtime"},
	)
	batchSize = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "ekiden_worker_batch_size",
			Help: "Number of transactions is a batch",
		},
		[]string{"runtime"},
	)
	roothashCommitLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "ekiden_worker_roothash_commit_latency",
			Help: "Latency of roothash commit",
		},
		[]string{"runtime"},
	)
	nodeCollectors = []prometheus.Collector{
		discrepancyDetectedCount,
		abortedBatchCount,
		storageCommitLatency,
		batchProcessingTime,
		batchRuntimeProcessingTime,
		batchSize,
		roothashCommitLatency,
	}

	metricsOnce sync.Once
)

// Config is a committee node configuration.
type Config struct {
	// TODO: Move this to common worker config.
	StorageCommitTimeout time.Duration

	ByzantineInjectDiscrepancies bool
}

// Node is a committee node.
type Node struct {
	commonNode *committee.Node
	mergeNode  *mergeCommittee.Node
	workerHost host.Host

	cfg Config

	ctx       context.Context
	cancelCtx context.CancelFunc
	stopCh    chan struct{}
	stopOnce  sync.Once
	quitCh    chan struct{}
	initCh    chan struct{}

	// Mutable and shared with common node's worker.
	// Guarded by .commonNode.CrossNode.
	state NodeState

	stateTransitions *pubsub.Broker
	// Bump this when we need to change what the worker selects over.
	reselect chan struct{}

	logger *logging.Logger
}

// Name returns the service name.
func (n *Node) Name() string {
	return "committee node"
}

// Start starts the service.
func (n *Node) Start() error {
	go n.worker()
	return nil
}

// Stop halts the service.
func (n *Node) Stop() {
	n.stopOnce.Do(func() { close(n.stopCh) })
}

// Quit returns a channel that will be closed when the service terminates.
func (n *Node) Quit() <-chan struct{} {
	return n.quitCh
}

// Cleanup performs the service specific post-termination cleanup.
func (n *Node) Cleanup() {
}

// Initialized returns a channel that will be closed when the node is
// initialized and ready to service requests.
func (n *Node) Initialized() <-chan struct{} {
	return n.initCh
}

// WatchStateTransitions subscribes to the node's state transitions.
func (n *Node) WatchStateTransitions() (<-chan NodeState, *pubsub.Subscription) {
	sub := n.stateTransitions.Subscribe()
	ch := make(chan NodeState)
	sub.Unwrap(ch)

	return ch, sub
}

func (n *Node) getMetricLabels() prometheus.Labels {
	return prometheus.Labels{
		"runtime": n.commonNode.RuntimeID.String(),
	}
}

// HandlePeerMessage implements NodeHooks.
func (n *Node) HandlePeerMessage(ctx context.Context, message *p2p.Message) (bool, error) {
	if message.LeaderBatchDispatch != nil {
		crash.Here(crashPointBatchReceiveAfter)

		bd := message.LeaderBatchDispatch
		err := n.queueBatchBlocking(ctx, bd.Batch, bd.Header)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (n *Node) queueBatchBlocking(ctx context.Context, batch runtime.Batch, hdr block.Header) error {
	// Quick check to see if header is compatible.
	if !bytes.Equal(hdr.Namespace[:], n.commonNode.RuntimeID) {
		n.logger.Warn("received incompatible header in external batch",
			"header", hdr,
		)
		return errIncomatibleHeader
	}

	var batchSpanCtx opentracing.SpanContext
	if batchSpan := opentracing.SpanFromContext(ctx); batchSpan != nil {
		batchSpanCtx = batchSpan.Context()
	}

	n.commonNode.CrossNode.Lock()
	defer n.commonNode.CrossNode.Unlock()
	return n.handleExternalBatchLocked(batch, batchSpanCtx, hdr)
}

// HandleBatchFromTransactionSchedulerLocked processes a batch from the transaction scheduler.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleBatchFromTransactionSchedulerLocked(batchSpanCtx opentracing.SpanContext, batch runtime.Batch) {
	n.maybeStartProcessingBatchLocked(batch, batchSpanCtx)
}

func (n *Node) bumpReselect() {
	select {
	case n.reselect <- struct{}{}:
	default:
		// If there's one already queued, we don't need to do anything.
	}
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) transitionLocked(state NodeState) {
	n.logger.Info("state transition",
		"current_state", n.state,
		"new_state", state,
	)

	// Validate state transition.
	dests := validStateTransitions[n.state.Name()]

	var valid bool
	for _, dest := range dests[:] {
		if dest == state.Name() {
			valid = true
			break
		}
	}

	if !valid {
		panic(fmt.Sprintf("invalid state transition: %s -> %s", n.state, state))
	}

	n.state = state
	n.stateTransitions.Broadcast(state)
	// Restart our worker's select in case our state-specific channels have changed.
	n.bumpReselect()
}

// HandleEpochTransitionLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleEpochTransitionLocked(epoch *committee.EpochSnapshot) {
	if epoch.IsComputeMember() {
		n.transitionLocked(StateWaitingForBatch{})
	} else {
		n.transitionLocked(StateNotReady{})
	}
}

// HandleNewBlockEarlyLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleNewBlockEarlyLocked(blk *block.Block) {
	crash.Here(crashPointRoothashReceiveAfter)
	// If we have seen a new block while a batch was processing, we need to
	// abort it no matter what as any processed state may be invalid.
	n.abortBatchLocked(errSeenNewerBlock)
}

// HandleNewBlockLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleNewBlockLocked(blk *block.Block) {
	header := blk.Header

	// Perform actions based on current state.
	switch state := n.state.(type) {
	case StateWaitingForBlock:
		// Check if this was the block we were waiting for.
		if header.MostlyEqual(state.header) {
			n.logger.Info("received block needed for batch processing")
			n.maybeStartProcessingBatchLocked(state.batch, state.batchSpanCtx)
			break
		}

		// Check if the new block is for the same or newer round than the
		// one we are waiting for. In this case, we should abort as the
		// block will never be seen.
		curRound := header.Round
		waitRound := state.header.Round
		if curRound >= waitRound {
			n.logger.Warn("seen newer block while waiting for block")
			n.transitionLocked(StateWaitingForBatch{})
			break
		}

		// Continue waiting for block.
		n.logger.Info("still waiting for block",
			"current_round", curRound,
			"wait_round", waitRound,
		)
	case StateWaitingForEvent:
		// Block finalized without the need for a backup worker.
		n.logger.Info("considering the round finalized",
			"round", blk.Header.Round,
			"header_hash", blk.Header.EncodedHash(),
		)
		n.transitionLocked(StateWaitingForBatch{})
	case StateWaitingForFinalize:
		// A new block means the round has been finalized.
		n.logger.Info("considering the round finalized",
			"round", blk.Header.Round,
			"header_hash", blk.Header.EncodedHash(),
		)
		n.transitionLocked(StateWaitingForBatch{})

		// Record time taken for successfully processing a batch.
		batchProcessingTime.With(n.getMetricLabels()).Observe(time.Since(state.batchStartTime).Seconds())
	}
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) maybeStartProcessingBatchLocked(batch runtime.Batch, batchSpanCtx opentracing.SpanContext) {
	epoch := n.commonNode.Group.GetEpochSnapshot()

	if epoch.IsComputeBackupWorker() {
		// Backup worker, wait for discrepancy event.
		n.transitionLocked(StateWaitingForEvent{
			batch:        batch,
			batchSpanCtx: batchSpanCtx,
		})
	} else {
		n.startProcessingBatchLocked(batch, batchSpanCtx)
	}
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) startProcessingBatchLocked(batch runtime.Batch, batchSpanCtx opentracing.SpanContext) {
	if n.commonNode.CurrentBlock == nil {
		panic("attempted to start processing batch with a nil block")
	}

	n.logger.Debug("processing batch",
		"batch", batch,
	)

	// Create batch processing context and channel for receiving the response.
	ctx, cancel := context.WithCancel(n.ctx)
	done := make(chan *protocol.ComputedBatch, 1)

	rq := &protocol.Body{
		WorkerExecuteTxBatchRequest: &protocol.WorkerExecuteTxBatchRequest{
			Calls: batch,
			Block: *n.commonNode.CurrentBlock,
		},
	}

	n.byzantineMaybeInjectDiscrepancy(rq.WorkerExecuteTxBatchRequest.Calls)

	batchStartTime := time.Now()
	batchSize.With(n.getMetricLabels()).Observe(float64(len(batch)))
	n.transitionLocked(StateProcessingBatch{batch, batchSpanCtx, batchStartTime, cancel, done})

	// Request the worker host to process a batch. This is done in a separate
	// goroutine so that the committee node can continue processing blocks.
	go func() {
		defer close(done)

		span := opentracing.StartSpan("CallBatch(rq)",
			opentracing.Tag{Key: "rq", Value: rq},
			opentracing.ChildOf(batchSpanCtx),
		)
		ctx = opentracing.ContextWithSpan(ctx, span)
		defer span.Finish()

		rtStartTime := time.Now()
		defer func() {
			batchRuntimeProcessingTime.With(n.getMetricLabels()).Observe(time.Since(rtStartTime).Seconds())
		}()

		ch, err := n.workerHost.MakeRequest(ctx, rq)
		if err != nil {
			n.logger.Error("error while sending batch processing request to worker host",
				"err", err,
			)
			return
		}
		crash.Here(crashPointBatchProcessStartAfter)

		select {
		case response := <-ch:
			if response == nil {
				n.logger.Error("worker channel closed while processing batch")
				return
			}

			rsp := response.WorkerExecuteTxBatchResponse
			if rsp == nil {
				n.logger.Error("malformed response from worker",
					"response", response,
				)
				return
			}

			done <- &rsp.Batch
		case <-ctx.Done():
			n.logger.Error("batch processing aborted by context, interrupting worker")

			// Interrupt the worker, so we can start processing the next batch.
			err = n.workerHost.InterruptWorker(n.ctx)
			if err != nil {
				n.logger.Error("failed to interrupt the worker",
					"err", err,
				)
			}
			return
		}
	}()
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) abortBatchLocked(reason error) {
	state, ok := n.state.(StateProcessingBatch)
	if !ok {
		// We can only abort if a batch is being processed.
		return
	}

	n.logger.Warn("aborting batch",
		"reason", reason,
	)

	// Cancel the batch processing context and wait for it to finish.
	state.cancel()

	crash.Here(crashPointBatchAbortAfter)

	// TODO: Return transactions to transaction scheduler.

	abortedBatchCount.With(n.getMetricLabels()).Inc()

	// After the batch has been aborted, we must wait for the round to be
	// finalized.
	n.transitionLocked(StateWaitingForFinalize{
		batchStartTime: state.batchStartTime,
	})
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) proposeBatchLocked(batch *protocol.ComputedBatch) {
	// We must be in ProcessingBatch state if we are here.
	state := n.state.(StateProcessingBatch)

	crash.Here(crashPointBatchProposeBefore)

	n.logger.Debug("proposing batch",
		"batch", batch,
	)

	epoch := n.commonNode.Group.GetEpochSnapshot()

	// Generate proposed compute results.
	proposedResults := &commitment.ComputeBody{
		CommitteeID: epoch.GetComputeCommitteeID(),
		Header:      batch.Header,
		RakSig:      batch.RakSig,
	}

	// Commit I/O and state write logs to storage.
	start := time.Now()
	err := func() error {
		span, ctx := tracing.StartSpanWithContext(n.ctx, "Apply(io, state)",
			opentracing.ChildOf(state.batchSpanCtx),
		)
		defer span.Finish()

		ctx, cancel := context.WithTimeout(ctx, n.cfg.StorageCommitTimeout)
		defer cancel()

		var emptyRoot hash.Hash
		emptyRoot.Empty()

		// NOTE: Order is important for verifying the receipt.
		applyOps := []storage.ApplyOp{
			// I/O root.
			storage.ApplyOp{Root: emptyRoot, ExpectedNewRoot: batch.Header.IORoot, WriteLog: batch.IOWriteLog},
			// State root.
			storage.ApplyOp{
				Root:            n.commonNode.CurrentBlock.Header.StateRoot,
				ExpectedNewRoot: batch.Header.StateRoot,
				WriteLog:        batch.StateWriteLog,
			},
		}

		signedReceipt, err := n.commonNode.Storage.ApplyBatch(ctx, applyOps)
		if err != nil {
			n.logger.Error("failed to apply to storage",
				"err", err,
			)
			return err
		}

		// TODO: Ensure that the receipt is actually signed by the
		// storage node.  For now accept a signature from anyone.
		var receipt storage.MKVSReceiptBody
		if err = signedReceipt.Open(&receipt); err != nil {
			n.logger.Error("failed to open signed receipt",
				"err", err,
			)
			return err
		}
		if err = proposedResults.VerifyStorageReceipt(&receipt); err != nil {
			n.logger.Error("failed to validate receipt",
				"err", err,
			)
			return err
		}

		// No need to append the entire blob, just the signature/public key.
		proposedResults.StorageReceipt = signedReceipt.Signature

		return nil
	}()
	storageCommitLatency.With(n.getMetricLabels()).Observe(time.Since(start).Seconds())

	if err != nil {
		n.abortBatchLocked(err)
		return
	}

	// Commit.
	commit, err := commitment.SignComputeCommitment(*n.commonNode.Identity.NodeKey, proposedResults)
	if err != nil {
		n.logger.Error("failed to sign commitment",
			"err", err,
		)
		n.abortBatchLocked(err)
		return
	}

	// Publish commitment to merge committee.
	spanPublish := opentracing.StartSpan("PublishComputeFinished(commitment)",
		opentracing.ChildOf(state.batchSpanCtx),
	)
	err = n.commonNode.Group.PublishComputeFinished(state.batchSpanCtx, commit)
	if err != nil {
		spanPublish.Finish()
		n.logger.Error("failed to publish results to committee",
			"err", err,
		)
		n.abortBatchLocked(err)
		return
	}
	spanPublish.Finish()

	// TODO: Add crash point.

	// TODO: Record commitment locally so we can submit it independently in case
	//       it is not included in a block.

	n.transitionLocked(StateWaitingForFinalize{
		batchStartTime: state.batchStartTime,
	})

	if epoch.IsMergeMember() {
		if n.mergeNode == nil {
			n.logger.Error("scheduler says we are a merge worker, but we are not")
		} else {
			n.mergeNode.HandleResultsFromComputeWorkerLocked(state.batchSpanCtx, commit)
		}
	}

	crash.Here(crashPointBatchProposeAfter)
}

// HandleNewEventLocked implements NodeHooks.
// Guarded by n.commonNode.CrossNode.
func (n *Node) HandleNewEventLocked(ev *roothash.Event) {
	dis := ev.ComputeDiscrepancyDetected
	if dis == nil {
		// Ignore other events.
		return
	}

	// If we are not waiting for an event, don't do anything.
	state, ok := n.state.(StateWaitingForEvent)
	if !ok {
		return
	}

	// TODO: Check if this is for our committee.

	n.logger.Warn("compute discrepancy detected",
		"committee_id", dis.CommitteeID,
	)

	crash.Here(crashPointDiscrepancyDetectedAfter)

	discrepancyDetectedCount.With(n.getMetricLabels()).Inc()

	if !n.commonNode.Group.GetEpochSnapshot().IsComputeBackupWorker() {
		return
	}

	// Backup worker, start processing a batch.
	n.logger.Info("backup worker activating and processing batch")
	n.startProcessingBatchLocked(state.batch, state.batchSpanCtx)
}

// Guarded by n.commonNode.CrossNode.
func (n *Node) handleExternalBatchLocked(batch runtime.Batch, batchSpanCtx opentracing.SpanContext, hdr block.Header) error {
	// If we are not waiting for a batch, don't do anything.
	if _, ok := n.state.(StateWaitingForBatch); !ok {
		return errIncorrectState
	}

	epoch := n.commonNode.Group.GetEpochSnapshot()

	// We can only receive external batches if we are a compute member.
	if !epoch.IsComputeMember() {
		n.logger.Error("got external batch while in incorrect role")
		return errIncorrectRole
	}

	// Check if we have the correct block -- in this case, start processing the batch.
	if n.commonNode.CurrentBlock.Header.MostlyEqual(&hdr) {
		n.maybeStartProcessingBatchLocked(batch, batchSpanCtx)
		return nil
	}

	// Check if the current block is older than what is expected we base our batch
	// on. In case it is equal or newer, but different, discard the batch.
	curRound := n.commonNode.CurrentBlock.Header.Round
	waitRound := hdr.Round
	if curRound >= waitRound {
		n.logger.Warn("got external batch based on incompatible header",
			"header", hdr,
		)
		return errIncomatibleHeader
	}

	// Wait for the correct block to arrive.
	n.transitionLocked(StateWaitingForBlock{
		batch:        batch,
		batchSpanCtx: batchSpanCtx,
		header:       &hdr,
	})

	return nil
}

func (n *Node) worker() {
	// Delay starting of committee node until after the consensus service
	// has finished initial synchronization, if applicable.
	if n.commonNode.Consensus != nil {
		n.logger.Info("delaying committee node start until after initial synchronization")
		select {
		case <-n.quitCh:
			return
		case <-n.commonNode.Consensus.Synced():
		}
	}
	n.logger.Info("starting committee node")

	defer close(n.quitCh)
	defer (n.cancelCtx)()

	// We are initialized.
	close(n.initCh)

	for {
		// Check if we are currently processing a batch. In this case, we also
		// need to select over the result channel.
		var processingDoneCh chan *protocol.ComputedBatch
		func() {
			n.commonNode.CrossNode.Lock()
			defer n.commonNode.CrossNode.Unlock()
			if stateProcessing, ok := n.state.(StateProcessingBatch); ok {
				processingDoneCh = stateProcessing.done
			}
		}()

		select {
		case batch := <-processingDoneCh:
			// Batch processing has finished.
			if batch == nil {
				n.logger.Warn("worker has aborted batch processing")
				func() {
					n.commonNode.CrossNode.Lock()
					defer n.commonNode.CrossNode.Unlock()
					n.abortBatchLocked(errWorkerAborted)
				}()
				break
			}

			n.logger.Info("worker has finished processing a batch")

			func() {
				n.commonNode.CrossNode.Lock()
				defer n.commonNode.CrossNode.Unlock()
				n.proposeBatchLocked(batch)
			}()
		case <-n.reselect:
			// Recalculate select set.
		}
	}
}

func NewNode(
	commonNode *committee.Node,
	mergeNode *mergeCommittee.Node,
	worker host.Host,
	cfg Config,
) (*Node, error) {
	metricsOnce.Do(func() {
		prometheus.MustRegister(nodeCollectors...)
	})

	ctx, cancel := context.WithCancel(context.Background())

	n := &Node{
		commonNode:       commonNode,
		mergeNode:        mergeNode,
		workerHost:       worker,
		cfg:              cfg,
		ctx:              ctx,
		cancelCtx:        cancel,
		stopCh:           make(chan struct{}),
		quitCh:           make(chan struct{}),
		initCh:           make(chan struct{}),
		state:            StateNotReady{},
		stateTransitions: pubsub.NewBroker(false),
		reselect:         make(chan struct{}, 1),
		logger:           logging.GetLogger("worker/compute/committee").With("runtime_id", commonNode.RuntimeID),
	}

	return n, nil
}
