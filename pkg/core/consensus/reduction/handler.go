package reduction

import (
	"bytes"

	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/committee"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/header"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/msg"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/user"
)

const maxCommitteeSize = 64

type (
	// ReductionHandler is responsible for performing operations that need to know
	// about specific event fields.
	reductionHandler struct {
		*committee.Handler
	}
)

// newReductionHandler will return a ReductionHandler, injected with the passed committee
// and an unmarshaller which uses the injected validation function.
func newReductionHandler(keys user.Keys, p user.Provisioners) *reductionHandler {
	return &reductionHandler{
		Handler: committee.NewHandler(keys, p),
	}
}

// AmMember checks if we are part of the committee.
func (b *reductionHandler) AmMember(round uint64, step uint8) bool {
	return b.Handler.AmMember(round, step, maxCommitteeSize)
}

func (b *reductionHandler) IsMember(pubKeyBLS []byte, round uint64, step uint8) bool {
	return b.Handler.IsMember(pubKeyBLS, round, step, maxCommitteeSize)
}

// func (b *reductionHandler) ExtractHeader(e wire.Event) *header.Header {
// 	ev := e.(*Reduction)
// 	return &header.Header{
// 		Round: ev.Round,
// 		Step:  ev.Step,
// 	}
// }

// func (b *reductionHandler) ExtractIdentifier(e wire.Event, r *bytes.Buffer) error {
// 	ev := e.(*Reduction)
// 	return encoding.Write256(r, ev.BlockHash)
// }

// Verify the BLS signature of the Reduction event.
func (b *reductionHandler) VerifySignature(hdr header.Header, sig []byte) error {
	info := new(bytes.Buffer)
	if err := header.MarshalSignableVote(info, hdr); err != nil {
		return err
	}

	return msg.VerifyBLSSignature(hdr.PubKeyBLS, info.Bytes(), sig)
}

func (b *reductionHandler) Quorum() int {
	return int(float64(b.CommitteeSize(maxCommitteeSize)) * 0.75)
}

// Committee returns a VotingCommittee for a given round and step.
func (b *reductionHandler) Committee(round uint64, step uint8) user.VotingCommittee {
	return b.Handler.Committee(round, step, maxCommitteeSize)
}
