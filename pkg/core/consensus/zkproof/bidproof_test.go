package zkproof_test

import (
	"fmt"
	"testing"

	ristretto "github.com/bwesterb/go-ristretto"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/zkproof"
)

func TestProofVerify(t *testing.T) {
	// amount in the bidding transaction
	d := genRandScalar()

	dAsBytes := d.Bytes()

	d = zkproof.BytesToScalar(dAsBytes)

	// secret number
	k := genRandScalar()
	// seed from block
	seed := genRandScalar()

	// public list of bids
	BidList := make([]ristretto.Scalar, 0, 5)
	for i := 0; i < 5; i++ {
		BidList = append(BidList, genRandScalar())
	}

	proof, qBytes, zBytes, pL := zkproof.Prove(d, k, seed, BidList)

	res := zkproof.Verify(proof, seed.Bytes(), pL, qBytes, zBytes)
	fmt.Println(res)
}

func genRandScalar() ristretto.Scalar {
	c := ristretto.Scalar{}
	c.Rand()
	return c
}
