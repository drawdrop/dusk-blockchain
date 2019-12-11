package chain

import (
	"bytes"
	"errors"
	"time"

	"encoding/binary"
	"fmt"
	"sync"

	"github.com/bwesterb/go-ristretto"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/peer/peermsg"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/peer/processing/chainsync"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/rpcbus"
	"github.com/dusk-network/dusk-wallet/block"
	"github.com/dusk-network/dusk-wallet/key"
	zkproof "github.com/dusk-network/dusk-zkproof"
	logger "github.com/sirupsen/logrus"

	cfg "github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/chain/candidate"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/user"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database/heavy"
	"github.com/dusk-network/dusk-blockchain/pkg/core/marshalling"
	"github.com/dusk-network/dusk-blockchain/pkg/core/verifiers"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/encoding"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	"github.com/dusk-network/dusk-wallet/transactions"
	"golang.org/x/crypto/ed25519"
)

var log *logger.Entry = logger.WithFields(logger.Fields{"process": "chain"})

// Chain represents the nodes blockchain
// This struct will be aware of the current state of the node.
type Chain struct {
	eventBus *eventbus.EventBus
	rpcBus   *rpcbus.RPCBus
	db       database.DB
	*candidateStore
	p       *user.Provisioners
	bidList *user.BidList
	counter *chainsync.Counter

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

	// collector channels
	certificateChan <-chan certMsg

	// rpcbus channels
	getLastBlockChan         <-chan rpcbus.Request
	verifyCandidateBlockChan <-chan rpcbus.Request
	getLastCertificateChan   <-chan rpcbus.Request
	getCandidateChan         <-chan rpcbus.Request
	getRoundResultsChan      <-chan rpcbus.Request
}

// New returns a new chain object
func New(eventBus *eventbus.EventBus, rpcBus *rpcbus.RPCBus, counter *chainsync.Counter) (*Chain, error) {
	_, db := heavy.CreateDBConnection()

	l, err := newLoader(db)
	if err != nil {
		return nil, fmt.Errorf("%s on loading chain db '%s'", err.Error(), cfg.Get().Database.Dir)
	}

	// set up collectors
	certificateChan := initCertificateCollector(eventBus)

	// set up rpcbus channels
	getLastBlockChan := make(chan rpcbus.Request, 1)
	verifyCandidateBlockChan := make(chan rpcbus.Request, 1)
	getLastCertificateChan := make(chan rpcbus.Request, 1)
	getCandidateChan := make(chan rpcbus.Request, 1)
	getRoundResultsChan := make(chan rpcbus.Request, 1)
	rpcBus.Register(rpcbus.GetLastBlock, getLastBlockChan)
	rpcBus.Register(rpcbus.VerifyCandidateBlock, verifyCandidateBlockChan)
	rpcBus.Register(rpcbus.GetLastCertificate, getLastCertificateChan)
	rpcBus.Register(rpcbus.GetCandidate, getCandidateChan)
	rpcBus.Register(rpcbus.GetRoundResults, getRoundResultsChan)

	chain := &Chain{
		eventBus:                 eventBus,
		rpcBus:                   rpcBus,
		db:                       db,
		candidateStore:           newCandidateStore(eventBus),
		prevBlock:                *l.chainTip,
		p:                        user.NewProvisioners(),
		bidList:                  &user.BidList{},
		counter:                  counter,
		certificateChan:          certificateChan,
		getLastBlockChan:         getLastBlockChan,
		verifyCandidateBlockChan: verifyCandidateBlockChan,
		getLastCertificateChan:   getLastCertificateChan,
		getCandidateChan:         getCandidateChan,
		getRoundResultsChan:      getRoundResultsChan,
	}

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

	chain.restoreConsensusData()

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
		case certMsg := <-c.certificateChan:
			c.handleCertificateMessage(certMsg)
		case r := <-c.getLastBlockChan:
			c.provideLastBlock(r)
		case r := <-c.verifyCandidateBlockChan:
			c.verifyCandidateBlock(r)
		case r := <-c.getLastCertificateChan:
			c.provideLastCertificate(r)
		case r := <-c.getCandidateChan:
			c.provideCandidate(r)
		case r := <-c.getRoundResultsChan:
			c.provideRoundResults(r)
		}
	}
}

func (c *Chain) propagateBlock(blk block.Block) error {
	buffer := topics.Block.ToBuffer()
	if err := marshalling.MarshalBlock(&buffer, &blk); err != nil {
		return err
	}

	c.eventBus.Publish(topics.Gossip, &buffer)
	return nil
}

func (c *Chain) addBidder(tx *transactions.Bid, startHeight uint64) {
	var bid user.Bid
	x := calculateXFromBytes(tx.Outputs[0].Commitment.Bytes(), tx.M)
	copy(bid.X[:], x.Bytes())
	copy(bid.M[:], tx.M)
	bid.EndHeight = startHeight + tx.Lock
	c.addBid(bid)
}

func (c *Chain) Close() error {
	log.Info("Close database")
	drvr, err := database.From(cfg.Get().Database.Driver)
	if err != nil {
		return err
	}

	return drvr.Close()
}

func (c *Chain) onAcceptBlock(m bytes.Buffer) error {
	// Ignore blocks from peers if we are only one behind - we are most
	// likely just about to finalize consensus.
	// TODO: we should probably just accept it if consensus was not
	// started yet
	if !c.counter.IsSyncing() {
		return nil
	}

	// If we are more than one block behind, stop the consensus
	c.eventBus.Publish(topics.StopConsensus, new(bytes.Buffer))

	// Accept the block
	blk := block.NewBlock()
	if err := marshalling.UnmarshalBlock(&m, blk); err != nil {
		return err
	}

	// This will decrement the sync counter
	if err := c.AcceptBlock(*blk); err != nil {
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
		return c.sendRoundUpdate()
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

	// 6. Cleanup obsolete candidate blocks
	l.Trace("cleaning obsolete candidate blocks")
	count := c.candidateStore.Clear(blk.Header.Height)
	log.WithField("count", count).Traceln("candidate blocks deleted")

	// 7. Remove expired provisioners and bids
	// We remove provisioners and bids from accepted block height + 2,
	// to set up our committee correctly for the next block.
	l.Trace("removing expired consensus transactions")
	c.removeExpiredProvisioners(blk.Header.Height + 2)
	c.removeExpiredBids(blk.Header.Height + 2)

	// 8. Notify other subsystems for the accepted block
	// Subsystems listening for this topic:
	// mempool.Mempool
	// consensus.generation.broker
	l.Trace("notifying internally")
	buf := new(bytes.Buffer)
	if err := marshalling.MarshalBlock(buf, &blk); err != nil {
		l.WithError(err).Errorln("block encoding failed")
		return err
	}

	c.eventBus.Publish(topics.AcceptedBlock, buf)

	l.Trace("procedure ended")
	return nil
}

func (c *Chain) onInitialization(bytes.Buffer) error {
	return c.sendRoundUpdate()
}

func (c *Chain) sendRoundUpdate() error {
	buf := new(bytes.Buffer)
	roundBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(roundBytes, c.intermediateBlock.Header.Height+1)
	if _, err := buf.Write(roundBytes); err != nil {
		return err
	}

	membersBuf, err := c.marshalProvisioners()
	if err != nil {
		return err
	}

	if _, err := buf.ReadFrom(membersBuf); err != nil {
		return err
	}

	if err := user.MarshalBidList(buf, *c.bidList); err != nil {
		return err
	}

	if err := encoding.WriteBLS(buf, c.intermediateBlock.Header.Seed); err != nil {
		return err
	}

	if err := encoding.Write256(buf, c.intermediateBlock.Header.Hash); err != nil {
		return err
	}

	c.eventBus.Publish(topics.RoundUpdate, buf)
	return nil
}

func (c *Chain) addConsensusNodes(txs []transactions.Transaction, startHeight uint64) {
	field := logger.Fields{"process": "accept block"}
	l := log.WithFields(field)

	for _, tx := range txs {
		switch tx.Type() {
		case transactions.StakeType:
			stake := tx.(*transactions.Stake)
			if err := c.addProvisioner(stake.PubKeyEd, stake.PubKeyBLS, stake.Outputs[0].EncryptedAmount.BigInt().Uint64(), startHeight, startHeight+stake.Lock-2); err != nil {
				l.Errorf("adding provisioner failed: %s", err.Error())
			}
		case transactions.BidType:
			bid := tx.(*transactions.Bid)
			c.addBidder(bid, startHeight)
		}
	}
}

func (c *Chain) verifyCandidateBlock(r rpcbus.Request) {
	// We need to verify the candidate block against the newest
	// intermediate block. The intermediate block would be the most
	// recent block before the candidate.
	if c.intermediateBlock == nil {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, errors.New("no intermediate block hash known")}
		return
	}

	candidateMsg, err := c.candidateStore.fetchCandidateMessage(r.Params.Bytes())
	if err != nil {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, errors.New("no candidate found for given hash")}
		return
	}

	err = verifiers.CheckBlock(c.db, *c.intermediateBlock, *candidateMsg.blk)
	r.RespChan <- rpcbus.Response{bytes.Buffer{}, err}
}

// Send Inventory message to all peers
func (c *Chain) advertiseBlock(b block.Block) error {
	msg := &peermsg.Inv{}
	msg.AddItem(peermsg.InvTypeBlock, b.Header.Hash)

	buf := new(bytes.Buffer)
	if err := msg.Encode(buf); err != nil {
		panic(err)
	}

	if err := topics.Prepend(buf, topics.Inv); err != nil {
		return err
	}

	c.eventBus.Publish(topics.Gossip, buf)
	return nil
}

// TODO: consensus data should be persisted to disk, to decrease
// startup times
func (c *Chain) restoreConsensusData() {
	var currentHeight uint64
	err := c.db.View(func(t database.Transaction) error {
		var err error
		currentHeight, err = t.FetchCurrentHeight()
		return err
	})

	if err != nil {
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
			break
		}

		for _, tx := range blk.Txs {
			switch t := tx.(type) {
			case *transactions.Stake:
				// Only add them if their stake is still valid
				if searchingHeight+t.Lock > currentHeight {
					amount := t.Outputs[0].EncryptedAmount.BigInt().Uint64()
					c.addProvisioner(t.PubKeyEd, t.PubKeyBLS, amount, searchingHeight+2, searchingHeight+t.Lock)
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
	return
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
func (c *Chain) addProvisioner(pubKeyEd, pubKeyBLS []byte, amount, startHeight, endHeight uint64) error {
	if len(pubKeyEd) != 32 {
		return fmt.Errorf("public key is %v bytes long instead of 32", len(pubKeyEd))
	}

	if len(pubKeyBLS) != 129 {
		return fmt.Errorf("public key is %v bytes long instead of 129", len(pubKeyBLS))
	}

	i := string(pubKeyBLS)
	stake := user.Stake{amount, startHeight, endHeight}

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
	m.PublicKeyEd = ed25519.PublicKey(pubKeyEd)

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

func (c *Chain) marshalProvisioners() (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	err := user.MarshalProvisioners(buf, c.p)
	return buf, err
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
	cm, err := c.candidateStore.fetchCandidateMessage(cMsg.hash)
	if err != nil {
		// If we don't have the block, we should wait for the network
		// to propagate it
		return
	}

	if c.intermediateBlock != nil {
		if err := c.finalizeIntermediateBlock(cm.cert); err != nil {
			log.WithError(err).Warnln("could not accept intermediate block")
			return
		}
	}

	// Set new intermediate block
	c.intermediateBlock = cm.blk

	// Notify mempool
	buf := new(bytes.Buffer)
	if err := marshalling.MarshalBlock(buf, cm.blk); err != nil {
		panic(err)
	}

	c.eventBus.Publish(topics.IntermediateBlock, buf)

	c.sendRoundUpdate()
}

func (c *Chain) finalizeIntermediateBlock(cert *block.Certificate) error {
	c.intermediateBlock.Header.Certificate = cert
	return c.AcceptBlock(*c.intermediateBlock)
}

// Send out a query for agreement messages and an intermediate block.
func (c *Chain) requestRoundResults(round uint64) (*block.Block, *block.Certificate, error) {
	roundResultsChan := make(chan bytes.Buffer, 10)
	id := c.eventBus.Subscribe(topics.RoundResults, eventbus.NewChanListener(roundResultsChan))
	defer c.eventBus.Unsubscribe(topics.RoundResults, id)

	buf := new(bytes.Buffer)
	if err := encoding.WriteUint64LE(buf, round); err != nil {
		panic(err)
	}

	if err := topics.Prepend(buf, topics.GetRoundResults); err != nil {
		panic(err)
	}

	c.eventBus.Publish(topics.Gossip, buf)
	// We wait 5 seconds for a response. We time out otherwise and
	// attempt catching up later.
	timer := time.NewTimer(5 * time.Second)

	for {
		select {
		case <-timer.C:
			return nil, nil, errors.New("request timeout")
		case b := <-roundResultsChan:
			blk := block.NewBlock()
			if err := marshalling.UnmarshalBlock(&b, blk); err != nil {
				// Prevent a malicious node from cutting us off by
				// sending garbled data
				continue
			}

			cert := block.EmptyCertificate()
			if err := marshalling.UnmarshalCertificate(&b, cert); err != nil {
				continue
			}

			// Check block and certificate for correctness
			if err := verifiers.CheckBlock(c.db, c.prevBlock, *blk); err != nil {
				continue
			}

			// Certificate needs to be on a block to be verified.
			// Since this certificate is supposed to be for the
			// intermediate block, we can just put it on there.
			blk.Header.Certificate = cert
			if err := verifiers.CheckBlockCertificate(*c.p, *blk); err != nil {
				continue
			}

			return blk, cert, nil
		}
	}
}

func (c *Chain) provideLastBlock(r rpcbus.Request) {
	buf := new(bytes.Buffer)

	c.mu.RLock()
	prevBlock := c.prevBlock
	c.mu.RUnlock()

	err := marshalling.MarshalBlock(buf, &prevBlock)
	r.RespChan <- rpcbus.Response{*buf, err}
}

func (c *Chain) provideLastCertificate(r rpcbus.Request) {
	if c.lastCertificate == nil {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, errors.New("no last certificate present")}
		return
	}

	buf := new(bytes.Buffer)
	err := marshalling.MarshalCertificate(buf, c.lastCertificate)
	r.RespChan <- rpcbus.Response{*buf, err}
}

func (c *Chain) provideCandidate(r rpcbus.Request) {
	cm, err := c.candidateStore.fetchCandidateMessage(r.Params.Bytes())
	if err != nil {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, errors.New("no candidate found for provided hash")}
	}

	buf := new(bytes.Buffer)
	err = encodeCandidateMessage(buf, cm)
	r.RespChan <- rpcbus.Response{*buf, err}
}

func (c *Chain) provideRoundResults(r rpcbus.Request) {
	if c.intermediateBlock == nil || c.lastCertificate == nil {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, errors.New("no intermediate block or certificate currently known")}
	}

	round := binary.LittleEndian.Uint64(r.Params.Bytes())
	if round != c.intermediateBlock.Header.Height {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, errors.New("no intermediate block and certificate for the given round")}
		return
	}

	buf := new(bytes.Buffer)
	if err := marshalling.MarshalBlock(buf, c.intermediateBlock); err != nil {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, err}
		return
	}

	if err := marshalling.MarshalCertificate(buf, c.lastCertificate); err != nil {
		r.RespChan <- rpcbus.Response{bytes.Buffer{}, err}
		return
	}

	r.RespChan <- rpcbus.Response{*buf, nil}
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

	// Credit a coinbase to an address generated from a zero seed.
	seed := make([]byte, 32)

	keyPair := key.NewKeyPair(seed)
	coinbaseTx, err := candidate.ConstructCoinbaseTx(keyPair.PublicKey(), make([]byte, 32), make([]byte, 32))
	if err != nil {
		return nil, err
	}

	blk.AddTx(coinbaseTx)
	if err := blk.SetRoot(); err != nil {
		return nil, err
	}

	if err := blk.SetHash(); err != nil {
		return nil, err
	}

	return blk, nil
}
