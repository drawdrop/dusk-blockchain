package voting

import (
	"bytes"
	"encoding/binary"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/notary"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/committee"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/reduction"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/user"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire/encoding"
)

type (
	collector struct {
		voteChannel   chan *bytes.Buffer
		unmarshalFunc func(*bytes.Buffer, signer) (wire.Event, error)
		signer        signer
	}

	eventSigner struct {
		*user.Keys
		committee committee.Committee
	}

	signer interface {
		committee.ReductionUnmarshaller
		addBLSPubKey(wire.Event)
		signBLS(wire.Event) error
		signEd25519([]byte) []byte
		EdPubKeyBytes() []byte
		eligibleToVote() bool
	}
)

func newEventSigner(keys *user.Keys, committee committee.Committee) *eventSigner {
	return &eventSigner{
		Keys:      keys,
		committee: committee,
	}
}

func initCollector(eventBus *wire.EventBus, topic string,
	unmarshalFunc func(*bytes.Buffer, signer) (wire.Event, error),
	signer signer) chan *bytes.Buffer {

	voteChannel := make(chan *bytes.Buffer, 1)
	collector := &collector{
		voteChannel:   voteChannel,
		unmarshalFunc: unmarshalFunc,
		signer:        signer,
	}
	go wire.NewEventSubscriber(eventBus, collector, topic).Accept()
	return voteChannel
}

func (c *collector) createVote(ev wire.Event) *bytes.Buffer {
	if !c.signer.eligibleToVote() {
		return nil
	}

	c.signer.addBLSPubKey(ev)
	c.signer.signBLS(ev)
	buffer := new(bytes.Buffer)
	c.signer.Marshal(buffer, ev)
	signature := c.signer.signEd25519(buffer.Bytes())
	return c.completeMessage(buffer.Bytes(), signature)
}

func (c *collector) completeMessage(marshalledEvent, signature []byte) *bytes.Buffer {
	buffer := bytes.NewBuffer(signature)
	if err := encoding.Write256(buffer, c.signer.EdPubKeyBytes()); err != nil {
		panic("signer has malformed keys")
	}

	if _, err := buffer.Write(marshalledEvent); err != nil {
		panic(err)
	}

	return buffer
}

func (c *collector) Collect(r *bytes.Buffer) error {
	info, err := c.unmarshalFunc(r, c.signer)
	if err != nil {
		return err
	}

	c.voteChannel <- c.createVote(info)
	return nil
}

func unmarshalBlockReduction(reductionBuffer *bytes.Buffer, signer signer) (wire.Event, error) {
	var round uint64
	if err := encoding.ReadUint64(reductionBuffer, binary.LittleEndian, &round); err != nil {
		return nil, err
	}

	var step uint8
	if err := encoding.ReadUint8(reductionBuffer, &step); err != nil {
		return nil, err
	}

	var votedHash []byte
	if err := encoding.Read256(reductionBuffer, &votedHash); err != nil {
		return nil, err
	}

	return &committee.ReductionEvent{
		EventHeader: &consensus.EventHeader{
			Round: round,
			Step:  step,
		},
		VotedHash: votedHash,
	}, nil
}

func unmarshalSigSetReduction(reductionBuffer *bytes.Buffer, signer signer) (wire.Event, error) {
	blockEvent, err := unmarshalBlockReduction(reductionBuffer, signer)
	if err != nil {
		return nil, err
	}

	var blockHash []byte
	if err := encoding.Read256(reductionBuffer, &blockHash); err != nil {
		return nil, err
	}

	return &reduction.SigSetEvent{
		ReductionEvent: blockEvent.(*committee.ReductionEvent),
		BlockHash:      blockHash,
	}, nil
}

func unmarshalBlockAgreement(agreementBuffer *bytes.Buffer, signer signer) (wire.Event, error) {
	var round uint64
	if err := encoding.ReadUint64(agreementBuffer, binary.LittleEndian, &round); err != nil {
		return nil, err
	}

	var step uint8
	if err := encoding.ReadUint8(agreementBuffer, &step); err != nil {
		return nil, err
	}

	var blockHash []byte
	if err := encoding.Read256(agreementBuffer, &blockHash); err != nil {
		return nil, err
	}

	voteSet, err := signer.UnmarshalVoteSet(agreementBuffer)
	if err != nil {
		return nil, err
	}

	return &notary.BlockEvent{
		EventHeader: &consensus.EventHeader{
			Round: round,
			Step:  step,
		},
		BlockHash: blockHash,
		VoteSet:   voteSet,
	}, nil
}

func unmarshalSigSetAgreement(agreementBuffer *bytes.Buffer, signer signer) (wire.Event, error) {
	agreement, err := unmarshalBlockAgreement(agreementBuffer, signer)
	if err != nil {
		return nil, err
	}

	var sigSetHash []byte
	if err := encoding.Read256(agreementBuffer, &sigSetHash); err != nil {
		return nil, err
	}

	return &notary.SigSetEvent{
		NotaryEvent: agreement.(*committee.NotaryEvent),
		SigSetHash:  sigSetHash,
	}, nil
}
