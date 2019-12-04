package peer

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/dusk-network/dusk-blockchain/pkg/p2p/peer/processing"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/protocol"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
)

// Handshake with another peer.
func (p *Writer) Handshake() error {

	if err := p.writeLocalMsgVersion(); err != nil {
		return err
	}

	if err := p.readVerAck(); err != nil {
		return err
	}

	if err := p.readRemoteMsgVersion(); err != nil {
		return err
	}

	return p.writeVerAck()
}

// Handshake with another peer.
func (p *Reader) Handshake() error {
	if err := p.readRemoteMsgVersion(); err != nil {
		return err
	}

	if err := p.writeVerAck(); err != nil {
		return err
	}

	if err := p.writeLocalMsgVersion(); err != nil {
		return err
	}

	return p.readVerAck()
}

func (p *Connection) writeLocalMsgVersion() error {
	message, err := p.createVersionBuffer()
	if err != nil {
		return err
	}

	if err := topics.Prepend(message, topics.Version); err != nil {
		return err
	}

	if err := processing.WriteFrame(message); err != nil {
		return err
	}

	magic := protocol.MagicFromConfig()
	buf := magic.ToBuffer()
	if _, err := buf.ReadFrom(message); err != nil {
		return err
	}

	_, err = p.Write(buf.Bytes())
	return err
}

func (p *Connection) readRemoteMsgVersion() error {
	msgBytes, err := p.ReadMessage()
	if err != nil {
		return err
	}

	decodedMsg := new(bytes.Buffer)
	decodedMsg.Write(msgBytes)

	topic, err := topics.Extract(decodedMsg)
	if err != nil {
		return err
	}

	if topic != topics.Version {
		return fmt.Errorf("did not receive the expected '%s' message - got %s",
			topics.Version, topic)
	}

	version, err := decodeVersionMessage(decodedMsg)
	if err != nil {
		return err
	}

	return verifyVersion(version.Version)
}

func (p *Connection) readVerAck() error {
	msgBytes, err := p.ReadMessage()
	if err != nil {
		return err
	}

	decodedMsg := new(bytes.Buffer)
	decodedMsg.Write(msgBytes)

	topic, err := topics.Extract(decodedMsg)
	if err != nil {
		return err
	}

	if topic != topics.VerAck {
		return fmt.Errorf("did not receive the expected '%s' message - got %s",
			topics.VerAck, topic)
	}

	return nil
}

func (p *Connection) writeVerAck() error {
	verAckMsg := new(bytes.Buffer)
	if err := topics.Prepend(verAckMsg, topics.VerAck); err != nil {
		return err
	}

	if err := processing.WriteFrame(verAckMsg); err != nil {
		return err
	}

	magic := protocol.MagicFromConfig()
	buf := magic.ToBuffer()
	if _, err := buf.ReadFrom(verAckMsg); err != nil {
		return err
	}

	if _, err := p.Write(buf.Bytes()); err != nil {
		return err
	}

	return nil
}

func (p *Connection) createVersionBuffer() (*bytes.Buffer, error) {
	version := protocol.NodeVer
	message, err := newVersionMessageBuffer(version, protocol.FullNode)
	if err != nil {
		return nil, err
	}

	return message, nil
}

func verifyVersion(v *protocol.Version) error {
	if protocol.NodeVer.Major != v.Major {
		return errors.New("version mismatch")
	}

	return nil
}
