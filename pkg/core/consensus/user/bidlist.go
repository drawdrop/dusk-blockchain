package user

import (
	"bytes"
	"errors"
	"math/rand"

	ristretto "github.com/bwesterb/go-ristretto"
	cfg "gitlab.dusk.network/dusk-core/dusk-go/pkg/config"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/block"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/database"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/transactions"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire/protocol"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/util/nativeutils/prerror"
	"gitlab.dusk.network/dusk-core/zkproof"
)

// Bid is the 32 byte X value, created from a bidding transaction amount and M.
type Bid [32]byte

// Equals will return whether or not the two bids are the same.
func (b Bid) Equals(bid Bid) bool {
	return bytes.Equal(b[:], bid[:])
}

// BidList is a list of bid X values.
type BidList []Bid

func NewBidList(db database.DB) (*BidList, error) {
	bl := &BidList{}
	if db == nil {
		drvr, err := database.From(cfg.Get().Database.Driver)
		if err != nil {
			return nil, err
		}

		db, err = drvr.Open(cfg.Get().Database.Dir, protocol.MagicFromConfig(), false)
		if err != nil {
			return nil, err
		}
	}

	bl.repopulate(db)
	return bl, nil
}

func (b *BidList) repopulate(db database.DB) {
	var currentHeight uint64
	err := db.View(func(t database.Transaction) error {
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
		err := db.View(func(t database.Transaction) error {
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
			bid, ok := tx.(*transactions.Bid)
			if !ok {
				continue
			}

			x := CalculateX(bid.Outputs[0].EncryptedAmount, bid.M)
			b.AddBid(x)
		}

		searchingHeight++
	}
}

// ReconstructBidListSubset will turn a slice of bytes into a BidList.
func ReconstructBidListSubset(pl []byte) (BidList, *prerror.PrError) {
	if len(pl)%32 != 0 {
		return nil, prerror.New(prerror.Low, errors.New("malformed bidlist"))
	}

	numBids := len(pl) / 32
	r := bytes.NewReader(pl)
	BidList := make(BidList, numBids)
	for i := 0; i < numBids; i++ {
		var bid Bid
		if _, err := r.Read(bid[:]); err != nil {
			return nil, prerror.New(prerror.High, err)
		}

		BidList[i] = bid
	}

	return BidList, nil
}

// ValidateBids will check if the passed BidList subset contains valid bids.
func (b *BidList) ValidateBids(bidListSubset BidList) *prerror.PrError {
loop:
	for _, x := range bidListSubset {
		for _, x2 := range *b {
			if x.Equals(x2) {
				continue loop
			}
		}

		return prerror.New(prerror.Low, errors.New("invalid public list"))
	}

	return nil
}

// Subset will shuffle the BidList, and returns a specified amount of
// bids from it.
func (b *BidList) Subset(amount int) []Bid {
	// Shuffle the public list
	list := *b
	rand.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })

	// Create our subset
	subset := make([]Bid, amount)
	for i := 0; i < amount; i++ {
		subset[i] = list[i]
	}

	return subset
}

// Contains checks if the BidList contains a specified Bid.
func (b *BidList) Contains(bid Bid) bool {
	for _, x := range *b {
		if x.Equals(bid) {
			return true
		}
	}

	return false
}

// AddBid will add a bid to the BidList.
func (b *BidList) AddBid(bid Bid) {
	// Check for duplicates
	for _, bidFromList := range *b {
		if bidFromList.Equals(bid) {
			return
		}
	}

	*b = append(*b, bid)
}

// RemoveBid will iterate over a BidList to remove a specified bid.
func (b *BidList) RemoveBid(bid Bid) {
	for i, bidFromList := range *b {
		if bidFromList.Equals(bid) {
			list := *b
			list = append(list[:i], list[i+1:]...)
			*b = list
		}
	}
}

func CalculateX(d []byte, m []byte) Bid {
	dScalar := ristretto.Scalar{}
	dScalar.UnmarshalBinary(d)

	mScalar := ristretto.Scalar{}
	mScalar.UnmarshalBinary(m)

	x := zkproof.CalculateX(dScalar, mScalar)

	var bid Bid
	copy(bid[:], x.Bytes()[:])
	return bid
}
