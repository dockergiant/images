package apm

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"github.com/vmihailenco/msgpack/v5"
	"github.com/vmihailenco/msgpack/v5/msgpcode"
	"google.golang.org/protobuf/encoding/protowire"
)

// The body cap alone does not bound memory: an empty span is one msgpack byte
// or two protobuf bytes but over a hundred bytes once decoded. These ceilings
// are counted before decoding. maxSpans is twice datadog.trace.spans_limit in
// roll-inspector-ini; a real payload of that size has about 1.2M elements.
const (
	maxSpans    = 40000
	maxElements = 2_000_000
	maxDepth    = 32
	maxJSONBody = 1 << 20
)

var errTooMany = errors.New("payload has too many elements")

// Parallel posts would add their decoded sizes up, so both intakes share two
// decode slots.
var decodeSlots = make(chan struct{}, 2)

func acquireSlot(r *http.Request) bool {
	select {
	case decodeSlots <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	}
}

func releaseSlot() { <-decodeSlots }

func decodeStatus(err error) int {
	if errors.Is(err, errTooMany) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

type budget struct {
	spans, elements int
}

func (b *budget) take(spans, elements int) error {
	b.spans += spans
	b.elements += elements
	if b.spans > maxSpans || b.elements > maxElements {
		return errTooMany
	}
	return nil
}

func checkDD(body []byte) error {
	dec := msgpack.NewDecoder(bytes.NewReader(body))
	n, err := dec.DecodeArrayLen()
	if err != nil {
		return err
	}
	var b budget
	if err := b.take(0, max(n, 0)); err != nil {
		return err
	}
	for range max(n, 0) {
		m, err := dec.DecodeArrayLen()
		if err != nil {
			return err
		}
		m = max(m, 0)
		if err := b.take(m, m); err != nil {
			return err
		}
		for range m {
			if err := walkMsgpack(dec, 2, &b); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkMsgpack(dec *msgpack.Decoder, depth int, b *budget) error {
	if depth > maxDepth {
		return errors.New("payload nests too deep")
	}
	c, err := dec.PeekCode()
	if err != nil {
		return err
	}
	var n, per int
	switch {
	case msgpcode.IsFixedArray(c) || c == msgpcode.Array16 || c == msgpcode.Array32:
		n, err = dec.DecodeArrayLen()
		per = 1
	case msgpcode.IsFixedMap(c) || c == msgpcode.Map16 || c == msgpcode.Map32:
		n, err = dec.DecodeMapLen()
		per = 2
	default:
		return dec.Skip()
	}
	if err != nil {
		return err
	}
	n = max(n, 0)
	if err := b.take(0, n); err != nil {
		return err
	}
	for range n * per {
		if err := walkMsgpack(dec, depth+1, b); err != nil {
			return err
		}
	}
	return nil
}

type pbMsg int

const (
	pbTraces pbMsg = iota
	pbResourceSpans
	pbResource
	pbScopeSpans
	pbScope
	pbSpan
	pbEvent
	pbLink
	pbKeyValue
	pbAnyValue
	pbArray
	pbKVList
)

// Every message-typed field becomes its own allocation in proto.Unmarshal,
// so the scan counts those; scalars and strings are bounded by the body.
var pbChildren = map[pbMsg]map[protowire.Number]pbMsg{
	pbTraces:        {1: pbResourceSpans},
	pbResourceSpans: {1: pbResource, 2: pbScopeSpans},
	pbResource:      {1: pbKeyValue},
	pbScopeSpans:    {1: pbScope, 2: pbSpan},
	pbScope:         {3: pbKeyValue},
	pbSpan:          {9: pbKeyValue, 11: pbEvent, 13: pbLink},
	pbEvent:         {3: pbKeyValue},
	pbLink:          {4: pbKeyValue},
	pbKeyValue:      {2: pbAnyValue},
	pbAnyValue:      {5: pbArray, 6: pbKVList},
	pbArray:         {1: pbAnyValue},
	pbKVList:        {1: pbKeyValue},
}

func checkOTLP(body []byte) error {
	var b budget
	return walkProto(body, pbTraces, 0, &b)
}

func walkProto(buf []byte, msg pbMsg, depth int, b *budget) error {
	if depth > maxDepth {
		return errors.New("payload nests too deep")
	}
	children := pbChildren[msg]
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return fmt.Errorf("protobuf: %w", protowire.ParseError(n))
		}
		buf = buf[n:]
		child, ok := children[num]
		if !ok || typ != protowire.BytesType {
			n = protowire.ConsumeFieldValue(num, typ, buf)
			if n < 0 {
				return fmt.Errorf("protobuf: %w", protowire.ParseError(n))
			}
			buf = buf[n:]
			continue
		}
		sub, n := protowire.ConsumeBytes(buf)
		if n < 0 {
			return fmt.Errorf("protobuf: %w", protowire.ParseError(n))
		}
		buf = buf[n:]
		spans := 0
		if child == pbSpan {
			spans = 1
		}
		if err := b.take(spans, 1); err != nil {
			return err
		}
		if err := walkProto(sub, child, depth+1, b); err != nil {
			return err
		}
	}
	return nil
}
