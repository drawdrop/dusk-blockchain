package payload

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"gitlab.dusk.network/dusk-core/dusk-go/crypto"
)

func TestMsgGetHeadersEncodeDecode(t *testing.T) {
	locator, err := crypto.RandEntropy(32)
	if err != nil {
		t.Fatal(err)
	}

	stop, err := crypto.RandEntropy(32)
	if err != nil {
		t.Fatal(err)
	}

	msg := NewMsgGetHeaders(locator, stop)
	buf := new(bytes.Buffer)
	if err := msg.Encode(buf); err != nil {
		t.Fatal(err)
	}

	msg2 := &MsgGetHeaders{}
	if err := msg2.Decode(buf); err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, msg, msg2)
}
