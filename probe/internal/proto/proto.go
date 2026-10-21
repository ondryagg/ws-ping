// Package proto implements the ondrya-latency.v1 wire format.
package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

const (
	Subprotocol = "ondrya-latency.v1"
	Version     = 1
)

var ErrMalformed = errors.New("malformed message")

type pingMsg struct {
	T   *string `json:"t"`
	Seq *uint32 `json:"seq"`
}

// ParsePing strictly decodes {"t":"ping","seq":<uint32>}. Unknown fields,
// missing fields, trailing data, negative, fractional or out-of-range seq
// are all errors.
func ParsePing(b []byte) (uint32, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m pingMsg
	if err := dec.Decode(&m); err != nil {
		return 0, ErrMalformed
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, ErrMalformed
	}
	if m.T == nil || *m.T != "ping" || m.Seq == nil {
		return 0, ErrMalformed
	}
	return *m.Seq, nil
}

// AppendPong appends {"t":"pong","seq":N}.
func AppendPong(dst []byte, seq uint32) []byte {
	dst = append(dst, `{"t":"pong","seq":`...)
	dst = strconv.AppendUint(dst, uint64(seq), 10)
	return append(dst, '}')
}

// HelloEncoder renders the hello frame with a fresh server time.
type HelloEncoder struct{ prefix []byte }

func NewHelloEncoder(node string, maxPings int) HelloEncoder {
	n, _ := json.Marshal(node) // a string cannot fail to marshal
	p := append([]byte(`{"t":"hello","proto":1,"node":`), n...)
	p = append(p, `,"maxPings":`...)
	p = strconv.AppendInt(p, int64(maxPings), 10)
	p = append(p, `,"serverTimeMs":`...)
	return HelloEncoder{prefix: p}
}

func (h HelloEncoder) Append(dst []byte, serverTimeMs int64) []byte {
	dst = append(dst, h.prefix...)
	dst = strconv.AppendInt(dst, serverTimeMs, 10)
	return append(dst, '}')
}
