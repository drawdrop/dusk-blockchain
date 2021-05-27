// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT License was not distributed with this
// file, you can obtain one at https://opensource.org/licenses/MIT.
//
// Copyright (c) DUSK NETWORK. All rights reserved.

package kadcast

import (
	"bytes"
	"errors"

	"github.com/dusk-network/dusk-blockchain/pkg/p2p/kadcast/encoding"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/message"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/protocol"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/sirupsen/logrus"
)

// Writer abstracts all of the logic and fields needed to write messages to
// other network nodes.
type Writer struct {
	subscriber eventbus.Subscriber
	gossip     *protocol.Gossip
	// Kademlia routing state
	router            *RoutingTable
	raptorCodeEnabled bool

	kadcastSubscription, kadcastPointSubscription uint32
}

// NewWriter returns a Writer. It will still need to be initialized by
// subscribing to the gossip topic with a stream handler, and by running the WriteLoop
// in a goroutine..
func NewWriter(router *RoutingTable, subscriber eventbus.Subscriber, gossip *protocol.Gossip, raptorCodeEnabled bool) *Writer {
	return &Writer{
		subscriber:        subscriber,
		router:            router,
		gossip:            gossip,
		raptorCodeEnabled: raptorCodeEnabled,
	}
}

// Serve processes any kadcast messaging to the wire.
func (w *Writer) Serve() {
	// NewChanListener is preferred here as it passes message.Message to the
	// Write, where NewStreamListener works with bytes.Buffer only.
	// Later this could be change if perf issue noticed.
	writeQueue := make(chan message.Message, 5000)
	w.kadcastSubscription = w.subscriber.Subscribe(topics.Kadcast, eventbus.NewChanListener(writeQueue))

	go func() {
		for msg := range writeQueue {
			if err := w.Write(msg); err != nil {
				log.WithError(err).Warn("kadcast write problem")
			}
		}
	}()

	writePointMsgQueue := make(chan message.Message, 5000)
	w.kadcastPointSubscription = w.subscriber.Subscribe(topics.KadcastPoint, eventbus.NewChanListener(writePointMsgQueue))

	go func() {
		for msg := range writePointMsgQueue {
			if err := w.WriteToPoint(msg); err != nil {
				log.WithError(err).Warn("kadcast-point writer problem")
			}
		}
	}()
}

// Write expects the actual payload in a marshaled form.
func (w *Writer) Write(m message.Message) error {
	header := m.Header()
	buf := m.Payload().(message.SafeBuffer)

	if len(header) == 0 {
		return errors.New("invalid message height")
	}

	// Constuct gossip frame.
	if err := w.gossip.Process(&buf.Buffer); err != nil {
		log.WithError(err).Error("reading gossip frame failed")
		return err
	}

	w.broadcastPacket(header[0], buf.Bytes())

	return nil
}

// WriteToPoint writes a message to a single destination.
// The receiver address is read from message Header.
func (w *Writer) WriteToPoint(m message.Message) error {
	// Height = 0 disables re-broadcast algorithm in the receiver node. That
	// said, sending a message to peer with height 0 will be received by the
	// destination peer but will not be repropagated to any other node.
	const height = byte(0)

	h := m.Header()
	if len(h) == 0 {
		return errors.New("empty header")
	}

	raddr := string(h)

	delegates := make([]encoding.PeerInfo, 1)

	var err error

	delegates[0], err = encoding.MakePeerFromAddr(raddr)
	if err != nil {
		return err
	}

	// Constuct gossip frame.
	buf := m.Payload().(message.SafeBuffer)
	if err = w.gossip.Process(&buf.Buffer); err != nil {
		log.WithError(err).Error("reading gossip frame failed")
		return err
	}

	// Marshal message data
	var packet []byte

	packet, err = w.marshalBroadcastPacket(height, buf.Bytes())
	if err != nil {
		log.WithError(err).Warn("marshal broadcast packet failed")
	}

	// Send message to a single destination using height = 0.
	w.sendToDelegates(delegates, height, packet)
	return nil
}

// BroadcastPacket sends a `CHUNKS` message across the network
// following the Kadcast broadcasting rules with the specified height.
func (w *Writer) broadcastPacket(maxHeight byte, payload []byte) {
	if maxHeight > byte(len(w.router.tree.buckets)) || maxHeight == 0 {
		return
	}

	if log.Logger.GetLevel() == logrus.TraceLevel {
		log.WithField("l_addr", w.router.LpeerInfo.String()).WithField("max_height", maxHeight).
			Traceln("broadcasting procedure")
	}

	// Marshal message data
	packet, err := w.marshalBroadcastPacket(0, payload)
	if err != nil {
		log.WithError(err).Warn("marshal broadcast packet failed")
	}

	for h := byte(0); h <= maxHeight-1; h++ {
		// Fetch delegating nodes based on height value
		delegates := w.fetchDelegates(h)

		// marshal binary once but adjust height field before each conn.Write
		packet[encoding.HeaderFixedLength] = h

		// Send to all delegates
		w.sendToDelegates(delegates, h, packet)
	}
}

func (w *Writer) marshalBroadcastPacket(h byte, payload []byte) ([]byte, error) {
	encHeader := makeHeader(encoding.BroadcastMsg, w.router)

	p := encoding.BroadcastPayload{
		Height:      h,
		GossipFrame: payload,
	}

	var buf bytes.Buffer
	if err := encoding.MarshalBinary(encHeader, &p, &buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (w *Writer) fetchDelegates(h byte) []encoding.PeerInfo {
	router := w.router
	myPeer := w.router.LpeerInfo

	// this should be always a deep copy of a bucket from the tree
	var b bucket

	router.tree.mu.RLock()
	b = router.tree.buckets[h]
	router.tree.mu.RUnlock()

	if len(b.entries) == 0 {
		return nil
	}

	delegates := make([]encoding.PeerInfo, 0)

	if b.idLength == 0 {
		// the bucket B 0 only holds one specific node of distance one
		for _, p := range b.entries {
			// Find neighbor peer
			if !myPeer.IsEqual(p) {
				delegates = append(delegates, p)
				break
			}
		}
	} else {
		// As per spec:
		//	Instead of having a single delegate per bucket, we select β
		//	delegates. This severely increases the probability that at least one
		//	out of the multiple selected nodes is honest and reachable.
		in := make([]encoding.PeerInfo, len(b.entries))
		copy(in, b.entries)

		delegates = getNoRandDelegates(router.beta, in)
		if len(delegates) == 0 {
			log.Warn("empty delegates list")
		}
	}

	return delegates
}

func (w *Writer) sendToDelegates(delegates []encoding.PeerInfo, height byte, packet []byte) {
	if len(delegates) == 0 {
		return
	}

	// For each of the delegates found from this bucket, make an attempt to
	// repropagate Broadcast message
	for _, destPeer := range delegates {
		if w.router.LpeerInfo.IsEqual(destPeer) {
			log.Error("Destination peer must be different from the source peer")
			continue
		}

		if log.Logger.GetLevel() == logrus.TraceLevel {
			// Avoid wasting CPU cycles for WithField construction in non-trace level
			log.WithField("l_addr", w.router.LpeerInfo.String()).
				WithField("r_addr", destPeer.String()).
				WithField("height", height).
				WithField("raptor", w.raptorCodeEnabled).
				WithField("len", len(packet)).
				Trace("Sending message")
		}

		// Send message to the dest peer with rc-udp or tcp
		if w.raptorCodeEnabled {
			// rc-udp write is destructive to the input message.
			// If more than one delegates are selected, duplicate the message.
			packetDup := make([]byte, len(packet))
			copy(packetDup, packet)

			go rcudpWrite(w.router.lpeerUDPAddr, destPeer.GetUDPAddr(), packetDup)
		} else {
			go tcpSend(destPeer.GetUDPAddr(), packet)
		}
	}
}

// Close unsubscribes from eventbus events.
func (w *Writer) Close() error {
	w.subscriber.Unsubscribe(topics.Kadcast, w.kadcastSubscription)
	w.subscriber.Unsubscribe(topics.KadcastPoint, w.kadcastPointSubscription)
	return nil
}
