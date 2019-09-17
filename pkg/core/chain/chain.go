package chain

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/dusk-network/dusk-blockchain/pkg/p2p/peer/peermsg"
	logger "github.com/sirupsen/logrus"

	cfg "github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/block"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/committee"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/msg"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/user"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database/heavy"
	"github.com/dusk-network/dusk-blockchain/pkg/core/verifiers"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	"github.com/dusk-network/dusk-blockchain/pkg/wallet/transactions"
)

var log *logger.Entry = logger.WithFields(logger.Fields{"process": "chain"})

// Chain represents the nodes blockchain
// This struct will be aware of the current state of the node.
type Chain struct {
	eventBus  *wire.EventBus
	rpcBus    *wire.RPCBus
	db        database.DB
	p         *user.Provisioners
	committee *committee.Agreement
	bidList   *user.BidList

	prevBlock block.Block
	// protect prevBlock with mutex as it's touched out of the main chain loop
	// by SubscribeCallback.
	// TODO: Consider if mutex can be removed
	mu sync.RWMutex

	// collector channels
	candidateChan   <-chan *block.Block
	certificateChan <-chan certMsg
}

// New returns a new chain object
func New(eventBus *wire.EventBus, rpcBus *wire.RPCBus) (*Chain, error) {
	_, db := heavy.CreateDBConnection()

	l, err := newLoader(db)
	if err != nil {
		return nil, fmt.Errorf("%s on loading chain db '%s'", err.Error(), cfg.Get().Database.Dir)
	}

	// set up collectors
	candidateChan := initBlockCollector(eventBus, string(topics.Candidate))
	certificateChan := initCertificateCollector(eventBus)

	// set up committee
	// set up bidlist
	bidList, err := user.NewBidList(db)
	if err != nil {
		return nil, err
	}

	chain := &Chain{
		eventBus:        eventBus,
		rpcBus:          rpcBus,
		db:              db,
		committee:       committee.NewAgreement(),
		bidList:         bidList,
		prevBlock:       *l.chainTip,
		candidateChan:   candidateChan,
		certificateChan: certificateChan,
	}
	if err := chain.newProvisioners(); err != nil {
		return nil, err
	}

	chain.updateCommittee()

	eventBus.SubscribeCallback(string(topics.Block), chain.onAcceptBlock)
	eventBus.RegisterPreprocessor(string(topics.Candidate), consensus.NewRepublisher(eventBus, topics.Candidate))
	return chain, nil
}

// Listen to the collectors
func (c *Chain) Listen() {
	for {
		select {

		case b := <-c.candidateChan:
			_ = c.handleCandidateBlock(*b)
		case cMsg := <-c.certificateChan:
			c.addCertificate(cMsg.hash, cMsg.cert)

		// wire.RPCBus requests handlers
		case r := <-wire.GetLastBlockChan:

			buf := new(bytes.Buffer)

			c.mu.RLock()
			prevBlock := c.prevBlock
			c.mu.RUnlock()

			if err := block.Marshal(buf, &prevBlock); err != nil {
				r.ErrChan <- err
				continue
			}

			r.RespChan <- *buf

		case r := <-wire.VerifyCandidateBlockChan:
			if err := c.verifyCandidateBlock(r.Params.Bytes()); err != nil {
				r.ErrChan <- err
				continue
			}

			r.RespChan <- bytes.Buffer{}
		}
	}
}

func (c *Chain) propagateBlock(blk block.Block) error {
	buffer := new(bytes.Buffer)
	if err := block.Marshal(buffer, &blk); err != nil {
		return err
	}

	msg, err := wire.AddTopic(buffer, topics.Block)
	if err != nil {
		return err
	}

	c.eventBus.Stream(string(topics.Gossip), msg)
	return nil
}

func (c *Chain) addBidder(tx *transactions.Bid, startHeight uint64) {
	x := user.CalculateX(tx.Outputs[0].Commitment.Bytes(), tx.M)
	x.EndHeight = startHeight + tx.Lock
	c.bidList.AddBid(x)
}

func (c *Chain) Close() error {

	log.Info("Close database")

	drvr, err := database.From(cfg.Get().Database.Driver)
	if err != nil {
		return err
	}

	return drvr.Close()
}

func (c *Chain) onAcceptBlock(m *bytes.Buffer) error {
	blk := block.NewBlock()
	if err := block.Unmarshal(m, blk); err != nil {
		return err
	}

	return c.AcceptBlock(*blk)
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

	l.Trace("procedure started")

	// 1. Check that stateless and stateful checks pass
	if err := verifiers.CheckBlock(c.db, c.prevBlock, blk); err != nil {
		l.Errorf("verification failed: %s", err.Error())
		return err
	}

	// 2. Check the certificate
	// This check should avoid a possible race condition between accepting two blocks
	// at the same height, as the probability of the committee creating two valid certificates
	// for the same round is negligible.
	if err := verifiers.CheckBlockCertificate(c.committee, blk); err != nil {
		l.Errorf("verifying the certificate failed: %s", err.Error())
		return err
	}

	// 3. Add provisioners and block generators
	c.addConsensusNodes(blk.Txs, blk.Header.Height)

	// 4. Store block in database
	err := c.db.Update(func(t database.Transaction) error {
		return t.StoreBlock(&blk)
	})

	if err != nil {
		l.Errorf("block storing failed: %s", err.Error())
		return err
	}

	c.prevBlock = blk

	// 5. Notify other subsystems for the accepted block
	// Subsystems listening for this topic:
	// mempool.Mempool
	// consensus.generation.broker
	buf := new(bytes.Buffer)
	if err := block.Marshal(buf, &blk); err != nil {
		l.Errorf("block encoding failed: %s", err.Error())
		return err
	}

	c.eventBus.Publish(string(topics.AcceptedBlock), buf)

	// 6. Gossip advertise block Hash
	if err := c.advertiseBlock(blk); err != nil {
		l.Errorf("block advertising failed: %s", err.Error())
		return err
	}

	// 7. Cleanup obsolete candidate blocks
	var count uint32
	err = c.db.Update(func(t database.Transaction) error {
		count, err = t.DeleteCandidateBlocks(blk.Header.Height)
		return err
	})

	if err != nil {
		// Not critical enough to abort the accepting procedure
		log.Warnf("DeleteCandidateBlocks failed with an error: %s", err.Error())
	} else {
		log.Infof("%d deleted candidate blocks", count)
	}

	// 8. Remove expired provisioners
	// We remove provisioners from accepted block height + 1,
	// to set up our committee correctly for the next block.
	// We also update our committee along with this.
	c.removeExpiredProvisioners(blk.Header.Height + 1)
	c.updateCommittee()

	// 9. Send round update
	// We send a round update after accepting a new block, which should include
	// a set of provisioners, and a bidlist. This allows the consensus components
	// to rehydrate their state properly for the next round.
	if err := c.sendRoundUpdate(blk.Header.Height + 1); err != nil {
		l.Errorf("sending round update failed: %s", err.Error())
		return err
	}

	l.Trace("procedure ended")
	return nil
}

func (c *Chain) sendRoundUpdate(round uint64) error {
	buf := new(bytes.Buffer)
	roundBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(roundBytes, round)
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
	// TODO: add bidlist buffer as well
	c.eventBus.Publish(msg.RoundUpdateTopic, buf)
	return nil
}

func (c *Chain) addConsensusNodes(txs []transactions.Transaction, startHeight uint64) {
	field := logger.Fields{"process": "accept block"}
	l := log.WithFields(field)

	for _, tx := range txs {
		switch tx.Type() {
		case transactions.StakeType:
			stake := tx.(*transactions.Stake)
			if err := c.addProvisioner(stake.PubKeyEd, stake.PubKeyBLS, stake.Outputs[0].EncryptedAmount.BigInt().Uint64(), startHeight+stake.Lock); err != nil {
				l.Errorf("adding provisioner failed: %s", err.Error())
			}
		case transactions.BidType:
			bid := tx.(*transactions.Bid)
			c.addBidder(bid, startHeight)
		}
	}
}

func (c *Chain) handleCandidateBlock(candidate block.Block) error {
	// Save it into persistent storage
	err := c.db.Update(func(t database.Transaction) error {
		return t.StoreCandidateBlock(&candidate)
	})

	if err != nil {
		log.Errorf("storing the candidate block failed: %s", err.Error())
		return err
	}

	return nil
}

func (c *Chain) handleWinningHash(blockHash []byte) error {
	// Fetch the candidate block that the winningHash points at
	candidate, err := c.fetchCandidateBlock(blockHash)
	if err != nil {
		log.Warnf("fetching a candidate block failed: %s", err.Error())
		return err
	}

	// Run the general procedure of block accepting
	return c.AcceptBlock(*candidate)
}

func (c *Chain) fetchCandidateBlock(hash []byte) (*block.Block, error) {
	var candidate *block.Block
	err := c.db.View(func(t database.Transaction) error {
		var err error
		candidate, err = t.FetchCandidateBlock(hash)
		return err
	})

	return candidate, err
}

func (c *Chain) verifyCandidateBlock(hash []byte) error {
	candidate, err := c.fetchCandidateBlock(hash)
	if err != nil {
		return err
	}

	return verifiers.CheckBlock(c.db, c.prevBlock, *candidate)
}

func (c *Chain) addCertificate(blockHash []byte, cert *block.Certificate) {
	candidate, err := c.fetchCandidateBlock(blockHash)
	if err != nil {
		log.Warnf("could not fetch candidate block to add certificate: %s", err.Error())
		return
	}

	candidate.Header.Certificate = cert
	c.AcceptBlock(*candidate)
}

// Send Inventory message to all peers
func (c *Chain) advertiseBlock(b block.Block) error {
	msg := &peermsg.Inv{}
	msg.AddItem(peermsg.InvTypeBlock, b.Header.Hash)

	buf := new(bytes.Buffer)
	if err := msg.Encode(buf); err != nil {
		panic(err)
	}

	withTopic, err := wire.AddTopic(buf, topics.Inv)
	if err != nil {
		return err
	}

	c.eventBus.Stream(string(topics.Gossip), withTopic)
	return nil
}

// newProvisioners returns an initialized Provisioners struct.
func (c *Chain) newProvisioners() error {
	p := user.NewProvisioners()
	c.p = p
	c.repopulateProvisioners()
	return nil
}

func (c *Chain) repopulateProvisioners() {
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
			stake, ok := tx.(*transactions.Stake)
			if !ok {
				continue
			}

			// Only add them if their stake is still valid
			if searchingHeight+stake.Lock > currentHeight {
				amount := stake.Outputs[0].EncryptedAmount.BigInt().Uint64()
				c.addProvisioner(stake.PubKeyEd, stake.PubKeyBLS, amount, searchingHeight+stake.Lock)
			}
		}

		searchingHeight++
	}
	return
}

// RemoveExpired removes Provisioners which stake expired
func (c *Chain) removeExpiredProvisioners(round uint64) uint64 {
	var totalRemoved uint64
	for pk, member := range c.p.Members {
		for i := 0; i < len(member.Stakes); i++ {
			if member.Stakes[i].EndHeight < round {
				totalRemoved += member.Stakes[i].Amount
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

	return totalRemoved
}

// addProvisioner will add a Member to the Provisioners by using the bytes of a BLS public key.
func (c *Chain) addProvisioner(pubKeyEd, pubKeyBLS []byte, amount, endHeight uint64) error {
	if len(pubKeyEd) != 32 {
		return fmt.Errorf("public key is %v bytes long instead of 32", len(pubKeyEd))
	}

	if len(pubKeyBLS) != 129 {
		return fmt.Errorf("public key is %v bytes long instead of 129", len(pubKeyBLS))
	}

	i := string(pubKeyBLS)
	stake := user.Stake{amount, endHeight}

	// Check for duplicates
	inserted := c.p.Set.Insert(pubKeyBLS)
	if !inserted {
		// If they already exist, just add their new stake
		c.p.Members[i].AddStake(stake)
		return nil
	}

	// This is a new provisioner, so let's initialize the Member struct and add them to the list
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
	members := c.sortProvisioners()
	buf := new(bytes.Buffer)
	err := user.MarshalMembers(buf, members)
	return buf, err
}

func (c *Chain) sortProvisioners() []user.Member {
	members := make([]user.Member, len(c.p.Members))
	for i := 0; i < len(c.p.Members); i++ {
		members[i] = *c.p.MemberAt(i)
	}

	return members
}

func (c *Chain) updateCommittee() {
	members := c.sortProvisioners()
	c.committee.Extractor.Stakers = user.NewStakers(members)
}
