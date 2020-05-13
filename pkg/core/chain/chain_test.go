package chain

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/block"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/transactions"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database/heavy"
	_ "github.com/dusk-network/dusk-blockchain/pkg/core/database/lite"
	"github.com/dusk-network/dusk-blockchain/pkg/core/tests/helper"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/peer/processing/chainsync"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/encoding"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/message"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/protocol"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/rpcbus"
	"github.com/stretchr/testify/assert"
)

// This test ensures the correct behavior from the Chain, when
// accepting a block from a peer.
func TestAcceptFromPeer(t *testing.T) {
	eb, _, c := setupChainTest(1, false)
	stopConsensusChan := make(chan message.Message, 1)
	eb.Subscribe(topics.StopConsensus, eventbus.NewChanListener(stopConsensusChan))

	streamer := eventbus.NewGossipStreamer(protocol.TestNet)
	eb.Subscribe(topics.Gossip, eventbus.NewStreamListener(streamer))

	// First, test accepting a block when the counter is set to not syncing.
	blk := helper.RandomBlock(1, 1)
	msg := message.New(topics.AcceptedBlock, *blk)

	assert.NoError(t, c.onAcceptBlock(msg))

	// Function should return before sending the `StopConsensus` message
	select {
	case <-stopConsensusChan:
		t.Fatal("not supposed to get a StopConsensus message")
	case <-time.After(1 * time.Second):
	}

	// Now, test accepting a block with 1 on the sync counter
	c.counter.StartSyncing(1)

	blk = mockAcceptableBlock(t, c.prevBlock)
	msg = message.New(topics.AcceptedBlock, *blk)

	errChan := make(chan error, 1)
	go func(chan error) {
		if err := c.onAcceptBlock(msg); err.Error() != "request timeout" {
			errChan <- err
		}
	}(errChan)

	// Should receive a StopConsensus message
	select {
	case err := <-errChan:
		t.Fatal(err)
	case <-stopConsensusChan:
	}

	// Discard block gossip
	if _, err := streamer.Read(); err != nil {
		t.Fatal(err)
	}

	// Should get a request for round results for round 2
	m, err := streamer.Read()
	if err != nil {
		t.Fatal(err)
	}

	if !assert.Equal(t, topics.GetRoundResults, streamer.SeenTopics()[1]) {
		t.FailNow()
	}

	var round uint64
	if err := encoding.ReadUint64LE(bytes.NewBuffer(m), &round); err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, uint64(2), round)
}

// This test ensures the correct behavior when accepting a block
// directly from the consensus.
func TestAcceptIntermediate(t *testing.T) {
	eb, rpc, c := setupChainTest(2, false)
	go c.Listen()
	intermediateChan := make(chan message.Message, 1)
	eb.Subscribe(topics.IntermediateBlock, eventbus.NewChanListener(intermediateChan))
	roundUpdateChan := make(chan message.Message, 1)
	eb.Subscribe(topics.RoundUpdate, eventbus.NewChanListener(roundUpdateChan))

	// Make a 'winning' candidate message
	blk := helper.RandomBlock(2, 1)
	cert := block.EmptyCertificate()
	provideCandidate(rpc, message.MakeCandidate(blk, cert))

	// Now send a `Certificate` message with this block's hash
	// Make a certificate with a different step, to do a proper equality
	// check later
	cert = block.EmptyCertificate()
	cert.Step = 5

	c.handleCertificateMessage(certMsg{blk.Header.Hash, cert, nil})

	// Should have `blk` as intermediate block now
	assert.True(t, blk.Equals(c.intermediateBlock))

	// lastCertificate should be `cert`
	assert.True(t, cert.Equals(c.lastCertificate))

	// Should have gotten `blk` over topics.IntermediateBlock
	blkMsg := <-intermediateChan
	decodedBlk := blkMsg.Payload().(block.Block)

	assert.True(t, decodedBlk.Equals(blk))

	// Should have gotten a round update with proper info
	ruMsg := <-roundUpdateChan
	ru := ruMsg.Payload().(consensus.RoundUpdate)
	// Should coincide with the new intermediate block
	assert.Equal(t, blk.Header.Height+1, ru.Round)
	assert.Equal(t, blk.Header.Hash, ru.Hash)
	assert.Equal(t, blk.Header.Seed, ru.Seed)
}

func TestReturnOnNilIntermediateBlock(t *testing.T) {
	eb, _, c := setupChainTest(2, false)
	intermediateChan := make(chan message.Message, 1)
	eb.Subscribe(topics.IntermediateBlock, eventbus.NewChanListener(intermediateChan))

	// Make a 'winning' candidate message
	blk := helper.RandomBlock(2, 1)
	cert := block.EmptyCertificate()

	cm := message.MakeCandidate(blk, cert)

	// Store it
	eb.Publish(topics.Candidate, message.New(topics.Candidate, cm))

	// Save current prevBlock
	currPrevBlock := c.prevBlock
	// set intermediate block to nil
	c.intermediateBlock = nil

	// Now pretend we finalized on it
	c.handleCertificateMessage(certMsg{blk.Header.Hash, cert, nil})

	// Ensure everything is still the same
	assert.True(t, currPrevBlock.Equals(&c.prevBlock))
	assert.Nil(t, c.intermediateBlock)
}

//nolint:unused
func provideCandidate(rpc *rpcbus.RPCBus, cm message.Candidate) {
	c := make(chan rpcbus.Request, 1)
	rpc.Register(topics.GetCandidate, c)

	go func() {
		r := <-c
		r.RespChan <- rpcbus.NewResponse(cm, nil)
	}()
}

//func createMockedCertificate(hash []byte, round uint64, keys []key.Keys, p *user.Provisioners) *block.Certificate {
//	votes := message.GenVotes(hash, round, 3, keys, p)
//	return &block.Certificate{
//		StepOneBatchedSig: votes[0].Signature.Compress(),
//		StepTwoBatchedSig: votes[1].Signature.Compress(),
//		Step:              1,
//		StepOneCommittee:  votes[0].BitSet,
//		StepTwoCommittee:  votes[1].BitSet,
//	}
//}

func createLoader() *DBLoader {
	_, db := heavy.CreateDBConnection()
	//genesis := cfg.DecodeGenesis()
	genesis := helper.RandomBlock(0, 12)
	return NewDBLoader(db, genesis)
}

func TestFetchTip(t *testing.T) {
	eb := eventbus.New()
	rpc := rpcbus.New()
	loader := createLoader()
	chain, err := New(context.Background(), eb, rpc, nil, loader, &MockVerifier{}, nil, nil)

	assert.Nil(t, err)

	// on a modern chain, state(tip) must point at genesis
	var s *database.State
	err = loader.db.View(func(t database.Transaction) error {
		s, err = t.FetchState()
		return err
	})

	assert.Nil(t, err)
	assert.Equal(t, chain.prevBlock.Header.Hash, s.TipHash)
}

func TestRebuildChain(t *testing.T) {
	// FIXME: 417 - rework this as soon as we have a viable block to mock Genesis
	/*
		eb, rb, c := setupChainTest(t, true)
		catchClearWalletDatabaseRequest(rb)
		go c.Listen()

		// Listen for `StopConsensus` messages
		stopConsensusChan := make(chan message.Message, 1)
		eb.Subscribe(topics.StopConsensus, eventbus.NewChanListener(stopConsensusChan))

		// Add a block so that we have a bit of chain state
		// to check against.
		blk := mockAcceptableBlock(t, c.prevBlock)

		assert.NoError(t, c.AcceptBlock(*blk))

		// Chain prevBlock should now no longer be genesis
		genesis := c.loader.(*DBLoader).genesis
		//genesis := cfg.DecodeGenesis()
		assert.False(t, genesis.Equals(&c.prevBlock))

		// Let's manually update some of the in-memory state, as it is
		// difficult to do this through mocked blocks in a test.
		p, ks := consensus.MockProvisioners(5)
		for _, k := range ks {
			assert.NoError(t, c.addProvisioner(k.BLSPubKeyBytes, 50000, 1, 2000))
		}

		c.lastCertificate = createMockedCertificate(c.intermediateBlock.Header.Hash, 2, ks, p)
		c.intermediateBlock = helper.RandomBlock(t, 2, 2)
		bids := make(user.BidList, 0)
		for i := 0; i < 3; i++ {
			bid := createBid(t)
			bids = append(bids, bid)
			*c.bidList = append(*c.bidList, bid)
		}

		// Now, send a request to rebuild the chain
		_, err := c.RebuildChain(context.Background(), &node.EmptyRequest{})
		assert.NoError(t, err)

		// We should be back at the genesis chain state
		assert.True(t, genesis.Equals(&c.prevBlock))
		for _, k := range ks {
			assert.Nil(t, c.p.GetMember(k.BLSPubKeyBytes))
		}

		assert.True(t, c.lastCertificate.Equals(block.EmptyCertificate()))
		intermediateBlock, err := mockFirstIntermediateBlock(c.prevBlock.Header)
		assert.NoError(t, err)
		assert.True(t, c.intermediateBlock.Equals(intermediateBlock))

		for _, bid := range bids {
			assert.False(t, c.bidList.Contains(bid))
		}

		// Ensure we got a `StopConsensus` message
		<-stopConsensusChan
	*/

}

//func createBid(t *testing.T) user.Bid {
//	b, err := crypto.RandEntropy(32)
//	if err != nil {
//		t.Fatal(err)
//	}
//
//	var arr [32]byte
//	copy(arr[:], b)
//	return user.Bid{X: arr, M: arr, EndHeight: 1000}
//}

//func catchClearWalletDatabaseRequest(rb *rpcbus.RPCBus) {
//	c := make(chan rpcbus.Request, 1)
//	rb.Register(topics.ClearWalletDatabase, c)
//	go func() {
//		r := <-c
//		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, nil)
//	}()
//}

// mock a block which can be accepted by the chain.
// note that this is only valid for height 1, as the certificate
// is not checked on height 1 (for network bootstrapping)
//nolint
func mockAcceptableBlock(t *testing.T, prevBlock block.Block) *block.Block {
	// Create block 1
	blk := helper.RandomBlock(1, 1)
	// Remove all txs except coinbase, as the helper transactions do not pass verification
	blk.Txs = blk.Txs[0:1]
	root, _ := blk.CalculateRoot()
	blk.Header.TxRoot = root
	hash, _ := blk.CalculateHash()
	blk.Header.Hash = hash
	// Add cert and prev hash
	blk.Header.Certificate = block.EmptyCertificate()
	blk.Header.PrevBlockHash = prevBlock.Header.Hash

	return blk
}

//nolint:unparam
func setupChainTest(startAtHeight uint64, includeGenesis bool) (*eventbus.EventBus, *rpcbus.RPCBus, *Chain) {
	eb := eventbus.New()
	rpc := rpcbus.New()
	counter := chainsync.NewCounter(eb)
	loader := createLoader()
	proxy := &transactions.MockProxy{
		E: transactions.MockExecutor(startAtHeight),
	}
	c, err := New(context.Background(), eb, rpc, counter, loader, &MockVerifier{}, nil, proxy.Executor())
	if err != nil {
		panic(err)
	}

	return eb, rpc, c
}
