package selection

import (
	"bytes"
	"errors"

	ristretto "github.com/bwesterb/go-ristretto"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/user"
	zkproof "github.com/dusk-network/dusk-zkproof"
)

type (
	scoreHandler struct {
		bidList user.BidList

		// Threshold number that a score needs to be greater than in order to be considered
		// for selection. Messages with scores lower than this threshold should not be
		// repropagated.
		threshold *consensus.Threshold
	}
)

func newScoreHandler(bidList user.BidList) *scoreHandler {
	return &scoreHandler{
		bidList:   bidList,
		threshold: consensus.NewThreshold(),
	}
}

func (sh *scoreHandler) ResetThreshold() {
	sh.threshold.Reset()
}

func (sh *scoreHandler) LowerThreshold() {
	sh.threshold.Lower()
}

// Priority returns true if the first element has priority over the second, false otherwise
func (sh *scoreHandler) Priority(first, second *ScoreEvent) bool {
	return bytes.Compare(second.Score, first.Score) != 1
}

func (sh *scoreHandler) Verify(m *ScoreEvent) error {
	// Check threshold
	if !sh.threshold.Exceeds(m.Score) {
		return errors.New("score does not exceed threshold")
	}

	// Check if the BidList contains valid bids
	if err := sh.validateBidListSubset(m.BidListSubset); err != nil {
		return err
	}

	// Verify the proof
	seedScalar := ristretto.Scalar{}
	seedScalar.Derive(m.Seed)

	proof := zkproof.ZkProof{
		Proof:         m.Proof,
		Score:         m.Score,
		Z:             m.Z,
		BinaryBidList: m.BidListSubset,
	}

	if !proof.Verify(seedScalar) {
		return errors.New("proof verification failed")
	}

	return nil
}

func (sh *scoreHandler) validateBidListSubset(bidListSubsetBytes []byte) error {
	bidListSubset, err := user.ReconstructBidListSubset(bidListSubsetBytes)
	if err != nil {
		return err
	}

	return sh.bidList.ValidateBids(bidListSubset)
}
