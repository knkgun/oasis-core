// Package tests si a collection of roothash implementation test cases.
package tests

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	beaconTests "github.com/oasisprotocol/oasis-core/go/beacon/tests"
	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/identity"
	"github.com/oasisprotocol/oasis-core/go/common/quantity"
	consensusAPI "github.com/oasisprotocol/oasis-core/go/consensus/api"
	registryTests "github.com/oasisprotocol/oasis-core/go/registry/tests"
	"github.com/oasisprotocol/oasis-core/go/roothash/api"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/block"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/commitment"
	"github.com/oasisprotocol/oasis-core/go/runtime/transaction"
	scheduler "github.com/oasisprotocol/oasis-core/go/scheduler/api"
	staking "github.com/oasisprotocol/oasis-core/go/staking/api"
	stakingTests "github.com/oasisprotocol/oasis-core/go/staking/tests"
	storageAPI "github.com/oasisprotocol/oasis-core/go/storage/api"
	"github.com/oasisprotocol/oasis-core/go/worker/storage"
)

const (
	recvTimeout = 5 * time.Second
	nrRuntimes  = 3
)

type runtimeState struct {
	id           string
	rt           *registryTests.TestRuntime
	genesisBlock *block.Block

	executorCommittee *testCommittee
	storageCommittee  *testCommittee
}

// RootHashImplementationTests exercises the basic functionality of a
// roothash backend.
func RootHashImplementationTests(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity) {
	seedBase := []byte(fmt.Sprintf("RootHashImplementationTests: %T", backend))

	require := require.New(t)

	// Ensure that we leave the registry empty when we are done.
	rtStates := make([]*runtimeState, 0, nrRuntimes)
	defer func() {
		if len(rtStates) > 0 {
			// This is entity deregistration based, and all of the
			// runtimes used in this test share the entity.
			rtStates[0].rt.Cleanup(t, consensus.Registry(), consensus)
		}

		registryTests.EnsureRegistryClean(t, consensus.Registry())
	}()

	// Populate the registry.
	runtimes := make([]*registryTests.TestRuntime, 0, nrRuntimes)
	for i := 0; i < nrRuntimes; i++ {
		t.Logf("Generating runtime: %d", i)
		seed := append([]byte{}, seedBase...)
		seed = append(seed, byte(i))

		rt, err := registryTests.NewTestRuntime(seed, nil, false)
		require.NoError(err, "NewTestRuntime")

		rtStates = append(rtStates, &runtimeState{
			id: strconv.Itoa(i),
			rt: rt,
		})
		runtimes = append(runtimes, rt)
	}
	registryTests.BulkPopulate(t, consensus.Registry(), consensus, runtimes, seedBase)

	t.Run("ConsensusParameters", func(t *testing.T) {
		testConsensusParameters(t, backend)
	})

	// Run the various tests. (Ordering matters)
	for _, v := range rtStates {
		t.Run("GenesisBlock/"+v.id, func(t *testing.T) {
			testGenesisBlock(t, backend, v)
		})
	}
	success := t.Run("EpochTransitionBlock", func(t *testing.T) {
		testEpochTransitionBlock(t, backend, consensus, rtStates)
	})
	if !success {
		return
	}

	// It only makes sense to run the following tests in case the
	// EpochTransitionBlock was successful. Otherwise this may leave the
	// committees set to nil and cause a crash.
	t.Run("SuccessfulRound", func(t *testing.T) {
		testSuccessfulRound(t, backend, consensus, identity, rtStates)
	})

	t.Run("RoundTimeout", func(t *testing.T) {
		testRoundTimeout(t, backend, consensus, identity, rtStates)
	})

	t.Run("ProposerTimeout", func(t *testing.T) {
		testProposerTimeout(t, backend, consensus, rtStates)
	})

	t.Run("RoundTimeoutWithEpochTransition", func(t *testing.T) {
		testRoundTimeoutWithEpochTransition(t, backend, consensus, identity, rtStates)
	})

	t.Run("EquivocationEvidence", func(t *testing.T) {
		testSubmitEquivocationEvidence(t, backend, consensus, identity, rtStates)
	})
}

func testConsensusParameters(t *testing.T, backend api.Backend) {
	ctx := context.Background()

	params, err := backend.ConsensusParameters(ctx, consensusAPI.HeightLatest)
	require.NoError(t, err, "ConsensusParameters")
	require.EqualValues(t, 32, params.MaxRuntimeMessages, "expected max runtime messages value")
}

func testGenesisBlock(t *testing.T, backend api.Backend, state *runtimeState) {
	require := require.New(t)

	id := state.rt.Runtime.ID
	ch, sub, err := backend.WatchBlocks(context.Background(), id)
	require.NoError(err, "WatchBlocks")
	defer sub.Close()

	var genesisBlock *block.Block
	select {
	case blk := <-ch:
		header := blk.Block.Header

		require.EqualValues(header.Version, 0, "block version")
		require.EqualValues(0, header.Round, "block round")
		require.Equal(block.Normal, header.HeaderType, "block header type")
		require.True(header.IORoot.IsEmpty(), "block I/O root empty")
		require.True(header.StateRoot.IsEmpty(), "block root hash empty")
		genesisBlock = blk.Block
	case <-time.After(recvTimeout):
		t.Fatalf("failed to receive block")
	}

	blk, err := backend.GetLatestBlock(context.Background(), &api.RuntimeRequest{
		RuntimeID: id,
		Height:    consensusAPI.HeightLatest,
	})
	require.NoError(err, "GetLatestBlock")
	require.EqualValues(genesisBlock, blk, "retreived block is genesis block")

	// We need to wait for the indexer to index the block. We could have a channel
	// to subscribe to these updates and this would not be needed.
	time.Sleep(1 * time.Second)

	blk, err = backend.GetGenesisBlock(context.Background(), &api.RuntimeRequest{
		RuntimeID: id,
		Height:    consensusAPI.HeightLatest,
	})
	require.NoError(err, "GetGenesisBlock")
	require.EqualValues(genesisBlock, blk, "retrieved block is genesis block")
}

func testEpochTransitionBlock(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, states []*runtimeState) {
	require := require.New(t)

	// Before an epoch transition there should just be a genesis block.
	for _, v := range states {
		genesisBlock, err := backend.GetLatestBlock(context.Background(), &api.RuntimeRequest{
			RuntimeID: v.rt.Runtime.ID,
			Height:    consensusAPI.HeightLatest,
		})
		require.NoError(err, "GetLatestBlock")
		require.EqualValues(0, genesisBlock.Header.Round, "genesis block round")

		v.genesisBlock = genesisBlock
	}

	// Subscribe to blocks for all of the runtimes.
	var blkChannels []<-chan *api.AnnotatedBlock
	for i := range states {
		v := states[i]
		ch, sub, err := backend.WatchBlocks(context.Background(), v.rt.Runtime.ID)
		require.NoError(err, "WatchBlocks")
		defer sub.Close()

		blkChannels = append(blkChannels, ch)
	}

	// Advance the epoch.
	timeSource := consensus.Beacon().(beacon.SetableBackend)
	beaconTests.MustAdvanceEpoch(t, timeSource)

	// Check for the expected post-epoch transition events.
	for i, state := range states {
		blkCh := blkChannels[i]
		state.testEpochTransitionBlock(t, consensus, blkCh)
	}

	// Check if GetGenesisBlock still returns the correct genesis block.
	for i := range states {
		blk, err := backend.GetGenesisBlock(context.Background(), &api.RuntimeRequest{
			RuntimeID: states[i].rt.Runtime.ID,
			Height:    consensusAPI.HeightLatest,
		})
		require.NoError(err, "GetGenesisBlock")
		require.EqualValues(0, blk.Header.Round, "retrieved block is genesis block")
	}
}

func (s *runtimeState) refreshCommittees(t *testing.T, consensus consensusAPI.Backend) {
	nodes := make(map[signature.PublicKey]*registryTests.TestNode)
	for _, node := range s.rt.TestNodes() {
		nodes[node.Node.ID] = node
	}

	epoch, err := consensus.Beacon().GetEpoch(context.Background(), consensusAPI.HeightLatest)
	require.NoError(t, err, "GetEpoch")

	s.executorCommittee, s.storageCommittee = mustGetCommittee(t, s.rt, epoch, consensus.Scheduler(), nodes)
}

func (s *runtimeState) testEpochTransitionBlock(t *testing.T, consensus consensusAPI.Backend, ch <-chan *api.AnnotatedBlock) {
	require := require.New(t)

	s.refreshCommittees(t, consensus)

	// Wait to receive an epoch transition block.
	for {
		select {
		case blk := <-ch:
			header := blk.Block.Header

			if header.HeaderType != block.EpochTransition {
				continue
			}

			require.True(header.IsParentOf(&s.genesisBlock.Header), "parent is parent of genesis block")
			require.True(header.IORoot.IsEmpty(), "block I/O root empty")
			require.EqualValues(s.genesisBlock.Header.StateRoot, header.StateRoot, "state root preserved")

			// Nothing more to do after the epoch transition block was received.
			return
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive block")
		}
	}
}

func testSuccessfulRound(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity, states []*runtimeState) {
	for _, state := range states {
		state.testSuccessfulRound(t, backend, consensus, identity)
	}
}

func (s *runtimeState) generateExecutorCommitments(t *testing.T, consensus consensusAPI.Backend, identity *identity.Identity, child *block.Block) (
	parent *block.Block,
	executorCommits []commitment.ExecutorCommitment,
	executorNodes []*registryTests.TestNode,
) {
	require := require.New(t)

	s.refreshCommittees(t, consensus)
	rt, executorCommittee := s.rt, s.executorCommittee

	dataDir, err := ioutil.TempDir("", "oasis-storage-test_")
	require.NoError(err, "TempDir")
	defer os.RemoveAll(dataDir)

	var ns common.Namespace
	copy(ns[:], rt.Runtime.ID[:])

	storageBackend, err := storage.NewLocalBackend(dataDir, ns, identity)
	require.NoError(err, "storage.New")
	defer storageBackend.Cleanup()

	// Generate a dummy I/O root.
	ioRoot := storageAPI.Root{
		Namespace: child.Header.Namespace,
		Version:   child.Header.Round + 1,
		Type:      storageAPI.RootTypeIO,
	}
	ioRoot.Hash.Empty()

	ctx := context.Background()
	tree := transaction.NewTree(nil, ioRoot)
	defer tree.Close()
	err = tree.AddTransaction(ctx, transaction.Transaction{Input: []byte("testInput"), Output: []byte("testOutput")}, nil)
	require.NoError(err, "tree.AddTransaction")
	ioWriteLog, ioRootHash, err := tree.Commit(ctx)
	require.NoError(err, "tree.Commit")

	var emptyRoot hash.Hash
	emptyRoot.Empty()

	// Create the new block header that the nodes will commit to.
	parent = &block.Block{
		Header: block.Header{
			Version:      0,
			Namespace:    child.Header.Namespace,
			Round:        child.Header.Round + 1,
			Timestamp:    block.Timestamp(time.Now().Unix()),
			HeaderType:   block.Normal,
			PreviousHash: child.Header.EncodedHash(),
			IORoot:       ioRootHash,
			StateRoot:    ioRootHash,
		},
	}
	require.True(parent.Header.IsParentOf(&child.Header), "parent is parent of child")
	parent.Header.StorageSignatures = mustStore(
		t,
		storageBackend,
		s.storageCommittee,
		child.Header.Namespace,
		child.Header.Round+1,
		[]storageAPI.ApplyOp{
			{RootType: storageAPI.RootTypeIO, SrcRound: child.Header.Round + 1, SrcRoot: emptyRoot, DstRoot: ioRootHash, WriteLog: ioWriteLog},
			// NOTE: Twice to get a receipt over both roots which we set to the same value.
			{RootType: storageAPI.RootTypeState, SrcRound: child.Header.Round, SrcRoot: emptyRoot, DstRoot: ioRootHash, WriteLog: ioWriteLog},
		},
	)

	var msgsHash hash.Hash
	msgsHash.Empty()

	// Generate all the executor commitments.
	executorNodes = append([]*registryTests.TestNode{}, executorCommittee.workers...)
	for _, node := range executorNodes {
		commitBody := commitment.ComputeBody{
			Header: commitment.ComputeResultsHeader{
				Round:        parent.Header.Round,
				PreviousHash: parent.Header.PreviousHash,
				IORoot:       &parent.Header.IORoot,
				StateRoot:    &parent.Header.StateRoot,
				MessagesHash: &msgsHash,
			},
			StorageSignatures: parent.Header.StorageSignatures,
			InputRoot:         hash.Hash{},
			InputStorageSigs:  []signature.Signature{},
		}

		// Fake txn scheduler signature.
		dispatch := &commitment.ProposedBatch{
			IORoot:            commitBody.InputRoot,
			StorageSignatures: commitBody.InputStorageSigs,
			Header:            child.Header,
		}

		// Get scheduler at round.
		var scheduler *scheduler.CommitteeNode
		scheduler, err = commitment.GetTransactionScheduler(s.executorCommittee.committee, child.Header.Round)
		require.NoError(err, "roothash.TransactionScheduler")
		// Get scheduler test node.
		var schedulerNode *registryTests.TestNode
		for _, node := range s.executorCommittee.workers {
			if node.Signer.Public().Equal(scheduler.PublicKey) {
				nd := node
				schedulerNode = nd
				break
			}
		}
		require.NotNil(schedulerNode, "TransactionScheduler missing in test nodes")

		var signedDispatch *commitment.SignedProposedBatch
		signedDispatch, err = commitment.SignProposedBatch(schedulerNode.Signer, s.rt.Runtime.ID, dispatch)
		require.NoError(err, "SignProposedBatch")
		commitBody.TxnSchedSig = signedDispatch.Signature

		// `err` shadows outside.
		commit, err := commitment.SignExecutorCommitment(node.Signer, s.rt.Runtime.ID, &commitBody) // nolint: vetshadow
		require.NoError(err, "SignExecutorCommitment")

		executorCommits = append(executorCommits, *commit)
	}
	return
}

func (s *runtimeState) testSuccessfulRound(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity) {
	require := require.New(t)

	child, err := backend.GetLatestBlock(context.Background(), &api.RuntimeRequest{
		RuntimeID: s.rt.Runtime.ID,
		Height:    consensusAPI.HeightLatest,
	})
	require.NoError(err, "GetLatestBlock")

	ch, sub, err := backend.WatchBlocks(context.Background(), s.rt.Runtime.ID)
	require.NoError(err, "WatchBlocks")
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel()

	// Generate and submit all executor commitments.
	parent, executorCommits, executorNodes := s.generateExecutorCommitments(t, consensus, identity, child)
	tx := api.NewExecutorCommitTx(0, nil, s.rt.Runtime.ID, executorCommits)
	err = consensusAPI.SignAndSubmitTx(ctx, consensus, executorNodes[0].Signer, tx)
	require.NoError(err, "ExecutorCommit")

	// Ensure that the round was finalized.
	for {
		select {
		case blk := <-ch:
			header := blk.Block.Header

			// Ensure that WatchBlocks uses the correct latest block.
			require.True(header.Round >= child.Header.Round, "WatchBlocks must start at child block")

			if header.Round == child.Header.Round {
				require.EqualValues(child.Header, header, "old block is equal")
				continue
			}

			// Can't directly compare headers, some backends rewrite the timestamp.
			require.EqualValues(parent.Header.Version, header.Version, "block version")
			require.EqualValues(parent.Header.Namespace, header.Namespace, "block namespace")
			require.EqualValues(parent.Header.Round, header.Round, "block round")
			// Timestamp
			require.EqualValues(parent.Header.HeaderType, header.HeaderType, "block header type")
			require.EqualValues(parent.Header.PreviousHash, header.PreviousHash, "block previous hash")
			require.EqualValues(parent.Header.IORoot, header.IORoot, "block I/O root")
			require.EqualValues(parent.Header.StateRoot, header.StateRoot, "block root hash")

			// There should be merge commitment events for all commitments.
			evts, err := backend.GetEvents(ctx, blk.Height)
			require.NoError(err, "GetEvents")
			// Executor commit event + Finalized event.
			require.Len(evts, len(executorCommits)+1, "should have all events")
			// First event is Finalized.
			fev := evts[0].Finalized
			require.EqualValues(header.Round, fev.Round, "finalized event should have the right round")
			require.Empty(fev.BadComputeNodes, "there should be no bad compute nodes")
			require.Len(fev.GoodComputeNodes, len(executorNodes), "all nodes should be good (round %d)", fev.Round)
			for i, ev := range evts[1:] {
				switch {
				case ev.ExecutorCommitted != nil:
					// Executor commitment event.
					require.EqualValues(executorCommits[i], ev.ExecutorCommitted.Commit, "executor commitment event should have the right commitment")
				default:
					// There should be no other event types.
					t.Fatalf("unexpected event: %+v", ev)
				}
			}

			// Nothing more to do after the block was received.
			return
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive block")
		}
	}
}

func testRoundTimeout(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity, states []*runtimeState) {
	for _, state := range states {
		state.testRoundTimeout(t, backend, consensus, identity)
	}
}

func (s *runtimeState) testRoundTimeout(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity) {
	require := require.New(t)

	child, err := backend.GetLatestBlock(context.Background(), &api.RuntimeRequest{
		RuntimeID: s.rt.Runtime.ID,
		Height:    consensusAPI.HeightLatest,
	})
	require.NoError(err, "GetLatestBlock")

	ch, sub, err := backend.WatchBlocks(context.Background(), s.rt.Runtime.ID)
	require.NoError(err, "WatchBlocks")
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel()

	// Only submit a single commitment to cause a timeout.
	_, executorCommits, executorNodes := s.generateExecutorCommitments(t, consensus, identity, child)
	tx := api.NewExecutorCommitTx(0, nil, s.rt.Runtime.ID, executorCommits[:1])
	err = consensusAPI.SignAndSubmitTx(ctx, consensus, executorNodes[0].Signer, tx)
	require.NoError(err, "ExecutorCommit")

	// Wait for RoundTimeout consensus blocks to pass.
	consBlkCh, consBlkSub, err := consensus.WatchBlocks(context.Background())
	require.NoError(err, "WatchBlocks")
	defer consBlkSub.Close()

	var startBlock int64
WaitForRoundTimeoutBlocks:
	for {
		select {
		case blk := <-consBlkCh:
			if blk == nil {
				t.Fatalf("block channel closed before reaching round timeout")
			}
			if startBlock == 0 {
				startBlock = blk.Height
			}
			// We wait for 2.5*RoundTimeout blocks as the first timeout will trigger discrepancy
			// resolution and the second timeout (slightly longer) will trigger a round failure.
			if blk.Height-startBlock > (25*s.rt.Runtime.Executor.RoundTimeout)/10 {
				break WaitForRoundTimeoutBlocks
			}
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive consensus block")
		}
	}

	// Ensure that the round failed due to a timeout.
	for {
		select {
		case blk := <-ch:
			header := blk.Block.Header

			// Skip initial round.
			if header.Round == child.Header.Round {
				continue
			}

			// Next round must be a failure.
			require.EqualValues(child.Header.Round+1, header.Round, "block round")
			require.EqualValues(block.RoundFailed, header.HeaderType, "block header type must be RoundFailed")

			// Nothing more to do after the block was received.
			return
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive runtime block")
		}
	}
}

func testRoundTimeoutWithEpochTransition(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity, states []*runtimeState) {
	for _, state := range states {
		state.testRoundTimeoutWithEpochTransition(t, backend, consensus, identity)
	}
}

func (s *runtimeState) testRoundTimeoutWithEpochTransition(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity) {
	require := require.New(t)

	child, err := backend.GetLatestBlock(context.Background(), &api.RuntimeRequest{
		RuntimeID: s.rt.Runtime.ID,
		Height:    consensusAPI.HeightLatest,
	})
	require.NoError(err, "GetLatestBlock")

	ch, sub, err := backend.WatchBlocks(context.Background(), s.rt.Runtime.ID)
	require.NoError(err, "WatchBlocks")
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel()

	// Only submit a single commitment to cause a timeout.
	_, executorCommits, executorNodes := s.generateExecutorCommitments(t, consensus, identity, child)
	tx := api.NewExecutorCommitTx(0, nil, s.rt.Runtime.ID, executorCommits[:1])
	err = consensusAPI.SignAndSubmitTx(ctx, consensus, executorNodes[0].Signer, tx)
	require.NoError(err, "ExecutorCommit")

	consBlkCh, consBlkSub, err := consensus.WatchBlocks(context.Background())
	require.NoError(err, "WatchBlocks")
	defer consBlkSub.Close()

	var startBlock int64
WaitForRoundTimeoutBlocks:
	for {
		select {
		case blk := <-consBlkCh:
			if blk == nil {
				t.Fatalf("block channel closed before reaching round timeout")
			}
			if startBlock == 0 {
				startBlock = blk.Height
			}
			if blk.Height-startBlock > s.rt.Runtime.Executor.RoundTimeout/2 {
				break WaitForRoundTimeoutBlocks
			}
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive consensus block")
		}
	}

	// Trigger an epoch transition while the timeout is armed.
	timeSource := consensus.Beacon().(beacon.SetableBackend)
	beaconTests.MustAdvanceEpoch(t, timeSource)

	// Ensure that the epoch transition was processed correctly.
	for {
		select {
		case blk := <-ch:
			header := blk.Block.Header

			// Skip initial rounds.
			if header.Round <= child.Header.Round {
				continue
			}

			// Next round must be an epoch transition.
			require.EqualValues(child.Header.Round+1, header.Round, "block round")
			require.EqualValues(block.EpochTransition, header.HeaderType, "block header type must be EpochTransition")

			// Nothing more to do after the block was received.
			return
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive runtime block")
		}
	}
}

func testProposerTimeout(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, states []*runtimeState) {
	for _, state := range states {
		state.testProposerTimeout(t, backend, consensus)
	}
}

func (s *runtimeState) testProposerTimeout(t *testing.T, backend api.Backend, consensus consensusAPI.Backend) {
	require := require.New(t)
	ctx := context.Background()

	child, err := backend.GetLatestBlock(ctx, &api.RuntimeRequest{
		RuntimeID: s.rt.Runtime.ID,
		Height:    consensusAPI.HeightLatest,
	})
	require.NoError(err, "GetLatestBlock")

	ch, sub, err := backend.WatchBlocks(context.Background(), s.rt.Runtime.ID)
	require.NoError(err, "WatchBlocks")
	defer sub.Close()

	// Wait for enough blocks so we can force trigger a timeout.
	consBlkCh, blocksSub, err := consensus.WatchBlocks(ctx)
	require.NoError(err, "consensus.WatchBlocks")
	defer blocksSub.Close()

	var startBlock int64
WaitForProposerTimeoutBlocks:
	for {
		select {
		case blk := <-consBlkCh:
			if blk == nil {
				t.Fatalf("block channel closed before reaching round timeout")
			}
			if startBlock == 0 {
				// XXX: Would be better to get the height of the latest roothash block,
				// and wait based on that. But we don't get that height unless we
				// Watch roothash blocks.
				startBlock = blk.Height
			}

			// Wait for enough blocks so that proposer timeout is allowed.
			if blk.Height >= startBlock+s.rt.Runtime.TxnScheduler.ProposerTimeout {
				break WaitForProposerTimeoutBlocks
			}
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive consensus block")
		}
	}

	// Get scheduler at round.
	var scheduler *scheduler.CommitteeNode
	scheduler, err = commitment.GetTransactionScheduler(s.executorCommittee.committee, child.Header.Round)
	require.NoError(err, "roothash.TransactionScheduler")

	// Select node to trigger timeout.
	var timeoutNode *registryTests.TestNode
	for _, node := range s.executorCommittee.workers {
		// Take first node that isn't the scheduler.
		if !node.Signer.Public().Equal(scheduler.PublicKey) {
			nd := node
			timeoutNode = nd
			break
		}
	}
	require.NotNil(timeoutNode, "No nodes that aren't transaction scheduler among test nodes")

	ctx, cancel := context.WithTimeout(ctx, recvTimeout)
	defer cancel()

	tx := api.NewRequestProposerTimeoutTx(0, nil, s.rt.Runtime.ID, child.Header.Round)
	err = consensusAPI.SignAndSubmitTx(ctx, consensus, timeoutNode.Signer, tx)
	require.NoError(err, "ExectutorTimeout")

	// Ensure that the round was finalized.
	for {
		select {
		case blk := <-ch:
			header := blk.Block.Header

			// Skip initial round.
			if header.Round == child.Header.Round {
				continue
			}

			// Next round must be a failure.
			require.EqualValues(child.Header.Round+1, header.Round, "block round")
			require.EqualValues(block.RoundFailed, header.HeaderType, "block header type must be RoundFailed")

			// Nothing more to do after the failed block was received.
			return
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive block")
		}
	}
}

type testCommittee struct {
	committee     *scheduler.Committee
	workers       []*registryTests.TestNode
	backupWorkers []*registryTests.TestNode
}

func mustGetCommittee(
	t *testing.T,
	rt *registryTests.TestRuntime,
	epoch beacon.EpochTime,
	sched scheduler.Backend,
	nodes map[signature.PublicKey]*registryTests.TestNode,
) (
	executorCommittee *testCommittee,
	storageCommittee *testCommittee,
) {
	require := require.New(t)

	ch, sub, err := sched.WatchCommittees(context.Background())
	require.NoError(err, "WatchCommittees")
	defer sub.Close()

	for {
		select {
		case committee := <-ch:
			if committee.ValidFor < epoch {
				continue
			}
			if !rt.Runtime.ID.Equal(&committee.RuntimeID) {
				continue
			}

			var ret testCommittee
			ret.committee = committee
			for _, member := range committee.Members {
				node := nodes[member.PublicKey]
				require.NotNil(node, "member is one of the nodes")
				switch member.Role {
				case scheduler.RoleWorker:
					ret.workers = append(ret.workers, node)
				case scheduler.RoleBackupWorker:
					ret.backupWorkers = append(ret.backupWorkers, node)
				}
			}

			var groupSize, groupBackupSize int
			switch committee.Kind {
			case scheduler.KindComputeExecutor:
				groupSize = int(rt.Runtime.Executor.GroupSize)
				groupBackupSize = int(rt.Runtime.Executor.GroupBackupSize)
			case scheduler.KindStorage:
				groupSize = int(rt.Runtime.Storage.GroupSize)
			}

			require.Len(ret.workers, groupSize, "workers exist")
			require.Len(ret.backupWorkers, groupBackupSize, "backup workers exist")

			switch committee.Kind {
			case scheduler.KindComputeExecutor:
				executorCommittee = &ret
			case scheduler.KindStorage:
				storageCommittee = &ret
			}

			if executorCommittee == nil || storageCommittee == nil {
				continue
			}

			return
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive committee event")
		}
	}
}

func mustStore(
	t *testing.T,
	store storageAPI.Backend,
	committee *testCommittee,
	ns common.Namespace,
	round uint64,
	ops []storageAPI.ApplyOp,
) []signature.Signature {
	require := require.New(t)

	receipts, err := store.ApplyBatch(context.Background(), &storageAPI.ApplyBatchRequest{
		Namespace: ns,
		DstRound:  round,
		Ops:       ops,
	})
	require.NoError(err, "ApplyBatch")
	require.NotEmpty(receipts, "ApplyBatch must return some storage receipts")

	// We need to fake the storage signatures as the storage committee under test
	// does not contain the key of the actual storage backend.

	var body storageAPI.ReceiptBody
	err = receipts[0].Open(&body)
	require.NoError(err, "Open")

	var signatures []signature.Signature
	for _, node := range committee.workers {
		var receipt *storageAPI.Receipt
		receipt, err = storageAPI.SignReceipt(node.Signer, ns, round, body.RootTypes, body.Roots)
		require.NoError(err, "SignReceipt")

		signatures = append(signatures, receipt.Signed.Signature)
	}
	return signatures
}

// MustTransitionEpoch waits till the roothash's view is past the epoch
// transition for a given epoch.
func MustTransitionEpoch(
	t *testing.T,
	runtimeID common.Namespace,
	roothash api.Backend,
	backend beacon.Backend,
	epoch beacon.EpochTime,
) {
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	defer cancel()

	blocksCh, sub, err := roothash.WatchBlocks(context.Background(), runtimeID)
	require.NoError(err, "WatchBlocks")
	defer sub.Close()

	// Wait for any block that's in the required epoch.  This is done
	// instead of specifically waiting for the epoch transition block
	// on the off chance that we are already past the epoch transition
	// block being broadcast.
	for {
		select {
		case annBlk := <-blocksCh:
			blkEpoch, err := backend.GetEpoch(ctx, annBlk.Height)
			require.NoError(err, "GetEpoch")
			if blkEpoch < epoch {
				continue
			}

			return
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive epoch transition block")
		}
	}
}

func testSubmitEquivocationEvidence(t *testing.T, backend api.Backend, consensus consensusAPI.Backend, identity *identity.Identity, states []*runtimeState) {
	require := require.New(t)

	ctx := context.Background()

	s := states[0]
	child, err := backend.GetLatestBlock(ctx, &api.RuntimeRequest{
		RuntimeID: s.rt.Runtime.ID,
		Height:    consensusAPI.HeightLatest,
	})
	require.NoError(err, "GetLatestBlock")

	// Generate and submit evidence of executor equivocation.
	if len(s.executorCommittee.workers) < 2 {
		t.Fatal("not enough executor nodes for running runtime misbehaviour evidence test")
	}

	// Generate evidence of executor equivocation.
	node := s.executorCommittee.workers[0]
	batch1 := &commitment.ProposedBatch{
		IORoot:            child.Header.IORoot,
		StorageSignatures: []signature.Signature{},
		Header:            child.Header,
	}
	signedBatch1, err := commitment.SignProposedBatch(node.Signer, s.rt.Runtime.ID, batch1)
	require.NoError(err, "SignProposedBatch")

	batch2 := &commitment.ProposedBatch{
		IORoot:            hash.NewFromBytes([]byte("different root")),
		StorageSignatures: []signature.Signature{},
		Header:            child.Header,
	}
	signedBatch2, err := commitment.SignProposedBatch(node.Signer, s.rt.Runtime.ID, batch2)
	require.NoError(err, "SignProposedBatch")

	ch, sub, err := consensus.Staking().WatchEvents(ctx)
	require.NoError(err, "staking.WatchEvents")
	defer sub.Close()

	// Ensure misbehaving node entity has some stake.
	entityAddress := staking.NewAddress(node.Node.EntityID)
	escrow := &staking.Escrow{
		Account: entityAddress,
		Amount:  *quantity.NewFromUint64(100),
	}
	tx := staking.NewAddEscrowTx(0, nil, escrow)
	err = consensusAPI.SignAndSubmitTx(ctx, consensus, stakingTests.Accounts.GetSigner(1), tx)
	require.NoError(err, "AddEscrow")

	// Submit evidence of executor equivocation.
	tx = api.NewEvidenceTx(0, nil, &api.Evidence{
		ID: s.rt.Runtime.ID,
		EquivocationBatch: &api.EquivocationBatchEvidence{
			BatchA: *signedBatch1,
			BatchB: *signedBatch2,
		},
	})
	submitter := s.executorCommittee.workers[1]
	err = consensusAPI.SignAndSubmitTx(ctx, consensus, submitter.Signer, tx)
	require.NoError(err, "SignAndSubmitTx(EvidenceTx)")

	// Wait for the node to get slashed.
WaitLoop:
	for {
		select {
		case ev := <-ch:
			if ev.Escrow == nil {
				continue
			}

			if e := ev.Escrow.Take; e != nil {
				require.EqualValues(entityAddress, e.Owner, "TakeEscrowEvent - owner must be entity's address")
				// All stake must be slashed as defined in debugGenesisState.
				require.EqualValues(escrow.Amount, e.Amount, "TakeEscrowEvent - all stake slashed")
				break WaitLoop
			}
		case <-time.After(recvTimeout):
			t.Fatalf("failed to receive slash event")
		}
	}

	// Ensure runtime acc got the slashed funds.
	runtimeAcc, err := consensus.Staking().Account(ctx, &staking.OwnerQuery{
		Height: consensusAPI.HeightLatest,
		Owner:  staking.NewRuntimeAddress(s.rt.Runtime.ID),
	})
	require.NoError(err, "staking.Account(runtimeAddr)")
	require.EqualValues(escrow.Amount, runtimeAcc.General.Balance, "Runtime account expected salshed balance")
}
