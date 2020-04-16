package chain

import (
	"bytes"
	"errors"
	"math/big"
	"time"

	"encoding/binary"
	"fmt"
	"sync"

	"github.com/bwesterb/go-ristretto"
	"github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/block"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/key"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/peer/peermsg"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/peer/processing/chainsync"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/rpcbus"
	"github.com/dusk-network/dusk-protobuf/autogen/go/node"
	zkproof "github.com/dusk-network/dusk-zkproof"
	logger "github.com/sirupsen/logrus"

	cfg "github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/user"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/transactions"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database/heavy"
	"github.com/dusk-network/dusk-blockchain/pkg/core/verifiers"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/encoding"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/message"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
)

var log = logger.WithFields(logger.Fields{"process": "chain"})

// Chain represents the nodes blockchain
// This struct will be aware of the current state of the node.
type Chain struct {
	eventBus *eventbus.EventBus
	rpcBus   *rpcbus.RPCBus
	db       database.DB
	p        *user.Provisioners
	bidList  *user.BidList
	counter  *chainsync.Counter

	prevBlock block.Block
	// protect prevBlock with mutex as it's touched out of the main chain loop
	// by SubscribeCallback.
	// TODO: Consider if mutex can be removed
	mu sync.RWMutex

	// Intermediate block, decided on by consensus.
	// Used to verify a candidate against the correct previous block,
	// and to be accepted once a certificate is decided on.
	intermediateBlock *block.Block

	// Most recent certificate generated by the Agreement component.
	// Held on the Chain, to be requested by the block generator,
	// for including it with the candidate message.
	lastCertificate *block.Certificate

	// The highest block we've seen from the network. This is updated
	// by the synchronizer, and used to calculate our synchronization
	// progress.
	highestSeen uint64

	// collector channels
	certificateChan <-chan certMsg
	highestSeenChan <-chan uint64

	// rpcbus channels
	getLastBlockChan         <-chan rpcbus.Request
	verifyCandidateBlockChan <-chan rpcbus.Request
	getLastCertificateChan   <-chan rpcbus.Request
	getRoundResultsChan      <-chan rpcbus.Request
	getSyncProgressChan      <-chan rpcbus.Request
	rebuildChainChan         <-chan rpcbus.Request
}

// New returns a new chain object
func New(eventBus *eventbus.EventBus, rpcBus *rpcbus.RPCBus, counter *chainsync.Counter) (*Chain, error) {
	_, db := heavy.CreateDBConnection()

	l, err := newLoader(db)
	if err != nil {
		return nil, fmt.Errorf("error on loading chain db '%s' - %v", cfg.Get().Database.Dir, err)
	}

	// set up collectors
	certificateChan := initCertificateCollector(eventBus)
	highestSeenChan := initHighestSeenCollector(eventBus)

	// set up rpcbus channels
	getLastBlockChan := make(chan rpcbus.Request, 1)
	verifyCandidateBlockChan := make(chan rpcbus.Request, 1)
	getLastCertificateChan := make(chan rpcbus.Request, 1)
	getRoundResultsChan := make(chan rpcbus.Request, 1)
	getSyncProgressChan := make(chan rpcbus.Request, 1)
	rebuildChainChan := make(chan rpcbus.Request, 1)
	if err := rpcBus.Register(topics.GetLastBlock, getLastBlockChan); err != nil {
		return nil, err
	}
	if err := rpcBus.Register(topics.VerifyCandidateBlock, verifyCandidateBlockChan); err != nil {
		return nil, err
	}
	if err := rpcBus.Register(topics.GetLastCertificate, getLastCertificateChan); err != nil {
		return nil, err
	}
	if err := rpcBus.Register(topics.GetRoundResults, getRoundResultsChan); err != nil {
		return nil, err
	}
	if err := rpcBus.Register(topics.GetSyncProgress, getSyncProgressChan); err != nil {
		return nil, err
	}
	if err := rpcBus.Register(topics.RebuildChain, rebuildChainChan); err != nil {
		return nil, err
	}

	chain := &Chain{
		eventBus:                 eventBus,
		rpcBus:                   rpcBus,
		db:                       db,
		prevBlock:                *l.chainTip,
		p:                        user.NewProvisioners(),
		bidList:                  &user.BidList{},
		counter:                  counter,
		certificateChan:          certificateChan,
		highestSeenChan:          highestSeenChan,
		getLastBlockChan:         getLastBlockChan,
		verifyCandidateBlockChan: verifyCandidateBlockChan,
		getLastCertificateChan:   getLastCertificateChan,
		getRoundResultsChan:      getRoundResultsChan,
		getSyncProgressChan:      getSyncProgressChan,
		rebuildChainChan:         rebuildChainChan,
	}

	// TODO feature-419: the Genesis block should be decoded by the startup
	// process and passed along wherever needed. This would guarantee
	// separation of concerns when testing
	// If the `prevBlock` is genesis, we add an empty intermediate block.
	genesis := cfg.DecodeGenesis()
	if bytes.Equal(chain.prevBlock.Header.Hash, genesis.Header.Hash) {
		// TODO: maybe it would be better to have a consensus-compatible
		// intermediate block and certificate.
		chain.lastCertificate = block.EmptyCertificate()
		blk, err := mockFirstIntermediateBlock(chain.prevBlock.Header)
		if err != nil {
			return nil, err
		}

		chain.intermediateBlock = blk
	}

	if err := chain.restoreConsensusData(); err != nil {
		log.WithError(err).Warnln("error in calling chain.restoreConsensusData from chain.New. The error is not propagated")
	}

	// Hook the chain up to the required topics
	cbListener := eventbus.NewCallbackListener(chain.onAcceptBlock)
	eventBus.Subscribe(topics.Block, cbListener)
	initListener := eventbus.NewCallbackListener(chain.onInitialization)
	eventBus.Subscribe(topics.Initialization, initListener)
	return chain, nil
}

// Listen to the collectors
func (c *Chain) Listen() {
	for {
		select {
		case certificateMsg := <-c.certificateChan:
			c.handleCertificateMessage(certificateMsg)
		case height := <-c.highestSeenChan:
			c.highestSeen = height
		case r := <-c.getLastBlockChan:
			c.provideLastBlock(r)
		case r := <-c.verifyCandidateBlockChan:
			c.processCandidateVerificationRequest(r)
		case r := <-c.getLastCertificateChan:
			c.provideLastCertificate(r)
		case r := <-c.getRoundResultsChan:
			c.provideRoundResults(r)
		case r := <-c.getSyncProgressChan:
			c.provideSyncProgress(r)
		case r := <-c.rebuildChainChan:
			c.rebuild(r)
		}
	}
}

func (c *Chain) addBidder(tx *transactions.Bid, startHeight uint64) {
	var bid user.Bid
	x := calculateXFromBytes(tx.Outputs[0].Commitment.Bytes(), tx.M)
	copy(bid.X[:], x.Bytes())
	copy(bid.M[:], tx.M)
	bid.EndHeight = startHeight + tx.Lock
	c.addBid(bid)
}

// Close the Chain DB
func (c *Chain) Close() error {
	log.Info("Close database")
	drvr, err := database.From(cfg.Get().Database.Driver)
	if err != nil {
		return err
	}

	return drvr.Close()
}

func (c *Chain) onAcceptBlock(m message.Message) error {
	// Ignore blocks from peers if we are only one behind - we are most
	// likely just about to finalize consensus.
	// TODO: we should probably just accept it if consensus was not
	// started yet
	if !c.counter.IsSyncing() {
		return nil
	}

	// If we are more than one block behind, stop the consensus
	c.eventBus.Publish(topics.StopConsensus, message.New(topics.StopConsensus, nil))

	// Accept the block
	blk := m.Payload().(block.Block)

	// This will decrement the sync counter
	if err := c.AcceptBlock(blk); err != nil {
		return err
	}

	// If we are no longer syncing after accepting this block,
	// request a certificate and intermediate block for the
	// second to last round.
	if !c.counter.IsSyncing() {
		blk, cert, err := c.requestRoundResults(blk.Header.Height + 1)
		if err != nil {
			return err
		}

		c.intermediateBlock = blk
		c.lastCertificate = cert

		// Once received, we can re-start consensus.
		// This sets off a chain of processing which goes from sending the
		// round update, to reinstantiating the consensus, to setting off
		// the first consensus loop. So, we do this in a goroutine to
		// avoid blocking other requests to the chain.
		go func() {
			_ = c.sendRoundUpdate()
		}()
	}

	return nil
}

// AcceptBlock will accept a block if
// 1. We have not seen it before
// 2. All stateless and statefull checks are true
// Returns nil, if checks passed and block was successfully saved
func (c *Chain) AcceptBlock(blk block.Block) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	field := logger.Fields{"process": "accept block"}
	l := log.WithFields(field)

	l.Trace("verifying block")

	// 1. Check that stateless and stateful checks pass
	if err := verifiers.CheckBlock(c.db, c.prevBlock, blk); err != nil {
		l.WithError(err).Warnln("block verification failed")
		return err
	}

	// 2. Check the certificate
	// This check should avoid a possible race condition between accepting two blocks
	// at the same height, as the probability of the committee creating two valid certificates
	// for the same round is negligible.
	l.Trace("verifying block certificate")
	if err := verifiers.CheckBlockCertificate(*c.p, blk); err != nil {
		l.WithError(err).Warnln("certificate verification failed")
		return err
	}

	// 3. Add provisioners and block generators
	l.Trace("adding consensus nodes")
	// We set the stake start height as blk.Header.Height+2.
	// This is because, once this block is accepted, the consensus will
	// be 2 rounds ahead of this current block height. As a result,
	// if we pick the start height to just be the block height, we
	// run into some inconsistencies when accepting the next block,
	// as the certificate could've been made with a different committee.
	c.addConsensusNodes(blk.Txs, blk.Header.Height+2)

	// 4. Store block in database
	l.Trace("storing block in db")
	err := c.db.Update(func(t database.Transaction) error {
		return t.StoreBlock(&blk)
	})

	if err != nil {
		l.WithError(err).Errorln("block storing failed")
		return err
	}

	c.prevBlock = blk

	// 5. Gossip advertise block Hash
	l.Trace("gossiping block")
	if err := c.advertiseBlock(blk); err != nil {
		l.WithError(err).Errorln("block advertising failed")
		return err
	}

	// 6. Remove expired provisioners and bids
	l.Trace("removing expired consensus transactions")
	c.removeExpiredProvisioners(blk.Header.Height)
	c.removeExpiredBids(blk.Header.Height + 2)

	// 7. Notify other subsystems for the accepted block
	// Subsystems listening for this topic:
	// mempool.Mempool
	// consensus.generation.broker
	l.Trace("notifying internally")

	msg := message.New(topics.AcceptedBlock, blk)
	c.eventBus.Publish(topics.AcceptedBlock, msg)

	l.Trace("procedure ended")
	return nil
}

func (c *Chain) onInitialization(message.Message) error {
	return c.sendRoundUpdate()
}

func (c *Chain) sendRoundUpdate() error {
	hdr := c.intermediateBlock.Header
	ru := consensus.RoundUpdate{
		Round:   hdr.Height + 1,
		P:       *c.p,
		BidList: *c.bidList,
		Seed:    hdr.Seed,
		Hash:    hdr.Hash,
	}
	msg := message.New(topics.RoundUpdate, ru)
	c.eventBus.Publish(topics.RoundUpdate, msg)
	return nil
}

func (c *Chain) addConsensusNodes(txs []transactions.Transaction, startHeight uint64) {
	field := logger.Fields{"process": "accept block"}
	l := log.WithFields(field)

	for _, tx := range txs {
		switch tx.Type() {
		case transactions.StakeType:
			stake := tx.(*transactions.Stake)
			if err := c.addProvisioner(stake.PubKeyBLS, stake.Outputs[0].EncryptedAmount.BigInt().Uint64(), startHeight, startHeight+stake.Lock-2); err != nil {
				l.Errorf("adding provisioner failed: %s", err.Error())
			}
		case transactions.BidType:
			bid := tx.(*transactions.Bid)
			c.addBidder(bid, startHeight)
		}
	}
}

func (c *Chain) processCandidateVerificationRequest(r rpcbus.Request) {
	// We need to verify the candidate block against the newest
	// intermediate block. The intermediate block would be the most
	// recent block before the candidate.
	if c.intermediateBlock == nil {
		r.RespChan <- rpcbus.Response{Resp: nil, Err: errors.New("no intermediate block hash known")}
		return
	}
	cm := r.Params.(message.Candidate)

	err := verifiers.CheckBlock(c.db, *c.intermediateBlock, *cm.Block)
	r.RespChan <- rpcbus.Response{Resp: nil, Err: err}
}

// Send Inventory message to all peers
func (c *Chain) advertiseBlock(b block.Block) error {
	msg := &peermsg.Inv{}
	msg.AddItem(peermsg.InvTypeBlock, b.Header.Hash)

	buf := new(bytes.Buffer)
	if err := msg.Encode(buf); err != nil {
		log.Panic(err)
	}

	if err := topics.Prepend(buf, topics.Inv); err != nil {
		log.Panic(err)
	}

	m := message.New(topics.Inv, *buf)
	c.eventBus.Publish(topics.Gossip, m)
	return nil
}

// TODO: consensus data should be persisted to disk, to decrease
// startup times
func (c *Chain) restoreConsensusData() error {
	var currentHeight uint64
	err := c.db.View(func(t database.Transaction) error {
		var err error
		currentHeight, err = t.FetchCurrentHeight()
		return err
	})

	if err != nil {
		log.WithError(err).Warnln("could not fetch current height from disk")
		currentHeight = 0
	}

	searchingHeight := uint64(0)
	if currentHeight > transactions.MaxLockTime {
		searchingHeight = currentHeight - transactions.MaxLockTime
	}

	for {
		var blk *block.Block
		err := c.db.View(func(t database.Transaction) error {
			hash, err := t.FetchBlockHashByHeight(searchingHeight)
			if err != nil {
				return err
			}

			blk, err = t.FetchBlock(hash)
			return err
		})

		if err != nil {
			log.WithError(err).Debugln("cannot fetch hash by heigth, quitting restoreConsensusData routine")
			break
		}

		for _, tx := range blk.Txs {
			switch t := tx.(type) {
			case *transactions.Stake:
				// Only add them if their stake is still valid
				if searchingHeight+t.Lock > currentHeight {
					amount := t.Outputs[0].EncryptedAmount.BigInt().Uint64()
					if err := c.addProvisioner(t.PubKeyBLS, amount, searchingHeight+2, searchingHeight+t.Lock); err != nil {
						return fmt.Errorf("unexpected error in adding provisioner following a stake transaction: %v", err)
					}
				}
			case *transactions.Bid:
				// TODO: The commitment to D is turned (in quite awful fashion) from a Point into a Scalar here,
				// to work with the `zkproof` package. Investigate if we should change this (reserve for testnet v2,
				// as this is most likely a consensus-breaking change)
				if searchingHeight+t.Lock > currentHeight {
					c.addBidder(t, searchingHeight)
				}
			}
		}

		searchingHeight++
	}

	return nil
}

// RemoveExpired removes Provisioners which stake expired
func (c *Chain) removeExpiredProvisioners(round uint64) {
	for pk, member := range c.p.Members {
		for i := 0; i < len(member.Stakes); i++ {
			if member.Stakes[i].EndHeight < round {
				member.RemoveStake(i)
				// If they have no stakes left, we should remove them entirely.
				if len(member.Stakes) == 0 {
					c.removeProvisioner([]byte(pk))
				}

				// Reset index
				i = -1
			}
		}
	}
}

// addProvisioner will add a Member to the Provisioners by using the bytes of a BLS public key.
func (c *Chain) addProvisioner(pubKeyBLS []byte, amount, startHeight, endHeight uint64) error {
	if len(pubKeyBLS) != 129 {
		return fmt.Errorf("public key is %v bytes long instead of 129", len(pubKeyBLS))
	}

	i := string(pubKeyBLS)
	stake := user.Stake{Amount: amount, StartHeight: startHeight, EndHeight: endHeight}

	// Check for duplicates
	_, inserted := c.p.Set.IndexOf(pubKeyBLS)
	if inserted {
		// If they already exist, just add their new stake
		c.p.Members[i].AddStake(stake)
		return nil
	}

	// This is a new provisioner, so let's initialize the Member struct and add them to the list
	c.p.Set.Insert(pubKeyBLS)
	m := &user.Member{}

	m.PublicKeyBLS = pubKeyBLS
	m.AddStake(stake)

	c.p.Members[i] = m
	return nil
}

// Remove a Member, designated by their BLS public key.
func (c *Chain) removeProvisioner(pubKeyBLS []byte) bool {
	delete(c.p.Members, string(pubKeyBLS))
	return c.p.Set.Remove(pubKeyBLS)
}

func calculateXFromBytes(d, m []byte) ristretto.Scalar {
	var dBytes [32]byte
	copy(dBytes[:], d)
	var mBytes [32]byte
	copy(mBytes[:], m)
	var dScalar ristretto.Scalar
	dScalar.SetBytes(&dBytes)
	var mScalar ristretto.Scalar
	mScalar.SetBytes(&mBytes)
	x := zkproof.CalculateX(dScalar, mScalar)
	return x
}

// AddBid will add a bid to the BidList.
func (c *Chain) addBid(bid user.Bid) {
	// Check for duplicates
	for _, bidFromList := range *c.bidList {
		if bidFromList.Equals(bid) {
			return
		}
	}

	*c.bidList = append(*c.bidList, bid)
}

// RemoveBid will iterate over a BidList to remove a specified bid.
func (c *Chain) removeBid(bid user.Bid) {
	for i, bidFromList := range *c.bidList {
		if bidFromList.Equals(bid) {
			c.bidList.Remove(i)
		}
	}
}

// RemoveExpired iterates over a BidList to remove expired bids.
func (c *Chain) removeExpiredBids(round uint64) {
	for _, bid := range *c.bidList {
		if bid.EndHeight < round {
			// We need to call RemoveBid here and loop twice, as the index
			// could be off if more than one bid is removed.
			c.removeBid(bid)
		}
	}
}

func (c *Chain) handleCertificateMessage(cMsg certMsg) {
	// Set latest certificate
	c.lastCertificate = cMsg.cert

	// Fetch new intermediate block and corresponding certificate
	resp, err := c.rpcBus.Call(topics.GetCandidate, rpcbus.NewRequest(*bytes.NewBuffer(cMsg.hash)), 5*time.Second)
	if err != nil {
		// If the we can't get the block, we will fall
		// back and catch up later.
		log.WithError(err).Warnln("could not find winning candidate block")
		return
	}
	cm := resp.(message.Candidate)

	if c.intermediateBlock == nil {
		// If we're missing the intermediate block, we will also fall
		// back and catch up later.
		log.Warnln("intermediate block is missing")
		return
	}

	if err := c.finalizeIntermediateBlock(cm.Certificate); err != nil {
		log.WithError(err).Warnln("could not accept intermediate block")
		return
	}

	// Set new intermediate block
	c.intermediateBlock = cm.Block

	// Notify mempool
	msg := message.New(topics.IntermediateBlock, *cm.Block)
	c.eventBus.Publish(topics.IntermediateBlock, msg)

	go func() {
		_ = c.sendRoundUpdate()
	}()
}

func (c *Chain) finalizeIntermediateBlock(cert *block.Certificate) error {
	c.intermediateBlock.Header.Certificate = cert
	return c.AcceptBlock(*c.intermediateBlock)
}

// Send out a query for agreement messages and an intermediate block.
func (c *Chain) requestRoundResults(round uint64) (*block.Block, *block.Certificate, error) {
	roundResultsChan := make(chan message.Message, 10)
	id := c.eventBus.Subscribe(topics.RoundResults, eventbus.NewChanListener(roundResultsChan))
	defer c.eventBus.Unsubscribe(topics.RoundResults, id)

	buf := new(bytes.Buffer)
	if err := encoding.WriteUint64LE(buf, round); err != nil {
		log.Panic(err)
	}

	// TODO: prepending the topic should be done at the recipient end of the
	// Gossip (together with all the other encoding)
	if err := topics.Prepend(buf, topics.GetRoundResults); err != nil {
		log.Panic(err)
	}
	msg := message.New(topics.GetRoundResults, *buf)
	c.eventBus.Publish(topics.Gossip, msg)
	// We wait 5 seconds for a response. We time out otherwise and
	// attempt catching up later.
	timer := time.NewTimer(5 * time.Second)

	for {
		select {
		case <-timer.C:
			return nil, nil, errors.New("request timeout")
		case m := <-roundResultsChan:
			cm := m.Payload().(message.Candidate)

			// Check block and certificate for correctness
			if err := verifiers.CheckBlock(c.db, c.prevBlock, *cm.Block); err != nil {
				continue
			}

			// Certificate needs to be on a block to be verified.
			// Since this certificate is supposed to be for the
			// intermediate block, we can just put it on there.
			cm.Block.Header.Certificate = cm.Certificate
			if err := verifiers.CheckBlockCertificate(*c.p, *cm.Block); err != nil {
				continue
			}

			return cm.Block, cm.Certificate, nil
		}
	}
}

func (c *Chain) provideLastBlock(r rpcbus.Request) {
	c.mu.RLock()
	prevBlock := c.prevBlock
	c.mu.RUnlock()
	r.RespChan <- rpcbus.NewResponse(prevBlock, nil)
}

func (c *Chain) provideLastCertificate(r rpcbus.Request) {
	if c.lastCertificate == nil {
		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, errors.New("no last certificate present"))
		return
	}

	buf := new(bytes.Buffer)
	err := message.MarshalCertificate(buf, c.lastCertificate)
	r.RespChan <- rpcbus.NewResponse(*buf, err)
}

func (c *Chain) provideRoundResults(r rpcbus.Request) {
	if c.intermediateBlock == nil || c.lastCertificate == nil {
		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, errors.New("no intermediate block or certificate currently known"))
		return
	}
	params := r.Params.(bytes.Buffer)

	if params.Len() < 8 {
		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, errors.New("round cannot be read from request param"))
		return
	}

	round := binary.LittleEndian.Uint64(params.Bytes())
	if round != c.intermediateBlock.Header.Height {
		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, errors.New("no intermediate block and certificate for the given round"))
		return
	}

	buf := new(bytes.Buffer)
	if err := message.MarshalBlock(buf, c.intermediateBlock); err != nil {
		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, err)
		return
	}

	if err := message.MarshalCertificate(buf, c.lastCertificate); err != nil {
		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, err)
		return
	}

	r.RespChan <- rpcbus.NewResponse(*buf, nil)
}

func (c *Chain) provideSyncProgress(r rpcbus.Request) {
	if c.highestSeen == 0 {
		r.RespChan <- rpcbus.NewResponse(&node.SyncProgressResponse{Progress: 0}, nil)
		return
	}

	c.mu.RLock()
	prevBlockHeight := c.prevBlock.Header.Height
	c.mu.RUnlock()

	progressPercentage := (float64(prevBlockHeight) / float64(c.highestSeen)) * 100

	// Avoiding strange output when the chain can be ahead of the highest
	// seen block, as in most cases, consensus terminates before we see
	// the new block from other peers.
	if progressPercentage > 100 {
		progressPercentage = 100
	}

	r.RespChan <- rpcbus.NewResponse(&node.SyncProgressResponse{Progress: float32(progressPercentage)}, nil)
}

// mocks an intermediate block with a coinbase attributed to a standard
// address. For use only when bootstrapping the network.
func mockFirstIntermediateBlock(prevBlockHeader *block.Header) (*block.Block, error) {
	blk := block.NewBlock()
	blk.Header.Seed = make([]byte, 33)
	blk.Header.Height = 1
	// Something above the genesis timestamp
	blk.Header.Timestamp = 1570000000
	blk.SetPrevBlock(prevBlockHeader)

	tx := mockDeterministicCoinbase()
	blk.AddTx(tx)
	root, err := blk.CalculateRoot()
	if err != nil {
		return nil, err
	}
	blk.Header.TxRoot = root

	hash, err := blk.CalculateHash()
	if err != nil {
		return nil, err
	}
	blk.Header.Hash = hash

	return blk, nil
}

func mockDeterministicCoinbase() transactions.Transaction {
	seed := make([]byte, 32)

	keyPair := key.NewKeyPair(seed)
	tx := transactions.NewCoinbase(make([]byte, 32), make([]byte, 32), 2)
	var r ristretto.Scalar
	r.SetZero()
	tx.SetTxPubKey(r)

	var reward ristretto.Scalar
	reward.SetBigInt(big.NewInt(int64(config.GeneratorReward)))

	_ = tx.AddReward(*keyPair.PublicKey(), reward)
	return tx
}

func (c *Chain) rebuild(r rpcbus.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Halt consensus
	msg := message.New(topics.StopConsensus, nil)
	c.eventBus.Publish(topics.StopConsensus, msg)

	// Remove EVERYTHING from the database. This includes the genesis
	// block, so we need to add it afterwards.
	err := c.db.Update(func(t database.Transaction) error {
		return t.ClearDatabase()
	})
	if err != nil {
		r.RespChan <- rpcbus.NewResponse(bytes.Buffer{}, err)
		return
	}

	// Note that, beyond this point, an error in reconstructing our
	// state is unrecoverable, as it deems the node totally useless.
	// Therefore, any error encountered from now on is answered by
	// a panic.

	// Load genesis into database and set the chain tip
	l, err := newLoader(c.db)
	if err != nil {
		log.Panic(err)
	}
	c.prevBlock = *l.chainTip

	// Reset in-memory values
	if err := c.resetState(); err != nil {
		log.Panic(err)
	}

	// Clear walletDB
	if _, err := c.rpcBus.Call(topics.ClearWalletDatabase, rpcbus.NewRequest(bytes.Buffer{}), 0*time.Second); err != nil {
		log.Panic(err)
	}

	r.RespChan <- rpcbus.NewResponse(&node.GenericResponse{Response: "Blockchain deleted. Syncing from scratch..."}, nil)
}

func (c *Chain) resetState() error {
	c.p = user.NewProvisioners()
	c.bidList = &user.BidList{}
	intermediateBlock, err := mockFirstIntermediateBlock(c.prevBlock.Header)
	if err != nil {
		return err
	}
	c.intermediateBlock = intermediateBlock

	c.lastCertificate = block.EmptyCertificate()
	if err := c.restoreConsensusData(); err != nil {
		log.WithError(err).Warnln("error in calling chain.restoreConsensusData from resetState. The error is not propagated")
	}
	return nil
}
