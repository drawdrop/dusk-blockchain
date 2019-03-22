package reduction

import (
	"bytes"
	"encoding/hex"
	"errors"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/committee"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/msg"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire/encoding"
)

type (
	blockEvent = Event

	reductionEventUnmarshaller struct {
		*consensus.EventHeaderUnmarshaller
	}

	// BlockHandler is responsible for performing operations that need to know
	// about specific event fields.
	BlockHandler struct {
		committee committee.Committee
		*reductionEventUnmarshaller
	}
)

func newReductionEventUnmarshaller(validate func(*bytes.Buffer) error) *reductionEventUnmarshaller {
	return &reductionEventUnmarshaller{
		EventHeaderUnmarshaller: consensus.NewEventHeaderUnmarshaller(validate),
	}
}

// Unmarshal unmarshals the buffer into a CommitteeEvent
func (a *reductionEventUnmarshaller) Unmarshal(r *bytes.Buffer, ev wire.Event) error {
	bev := ev.(*blockEvent)
	if err := a.EventHeaderUnmarshaller.Unmarshal(r, bev.EventHeader); err != nil {
		return err
	}

	if err := encoding.Read256(r, &bev.VotedHash); err != nil {
		return err
	}

	if err := encoding.ReadBLS(r, &bev.SignedHash); err != nil {
		return err
	}

	return nil
}

// NewBlockHandler will return a BlockHandler, injected with the passed committee
// and an unmarshaller which uses the injected validation function.
func NewBlockHandler(committee committee.Committee,
	validateFunc func(*bytes.Buffer) error) *BlockHandler {

	return &BlockHandler{
		committee:                  committee,
		reductionEventUnmarshaller: newReductionEventUnmarshaller(validateFunc),
	}
}

// NewEvent returns a blockEvent
func (b BlockHandler) NewEvent() wire.Event {
	return &blockEvent{}
}

// Stage returns the round and step of the passed blockEvent
func (b BlockHandler) Stage(e wire.Event) (uint64, uint8) {
	ev, ok := e.(*blockEvent)
	if !ok {
		return 0, 0
	}

	return ev.Round, ev.Step
}

// Hash returns the voted hash on the passed blockEvent
func (b BlockHandler) Hash(e wire.Event) string {
	ev, ok := e.(*blockEvent)
	if !ok {
		return ""
	}

	return hex.EncodeToString(ev.VotedHash)
}

// Verify the blockEvent
func (b BlockHandler) Verify(e wire.Event) error {
	ev, ok := e.(*blockEvent)
	if !ok {
		return errors.New("block handler: type casting error")
	}

	if err := msg.VerifyBLSSignature(ev.PubKeyBLS, ev.VotedHash, ev.SignedHash); err != nil {
		return err
	}

	if !b.committee.IsMember(ev.PubKeyBLS) {
		return errors.New("block handler: voter not eligible to vote")
	}

	return nil
}
