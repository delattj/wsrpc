package wsrpc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"golang.org/x/net/websocket"
	"log"
)

// Stream packet (implement message interface)

type stream_packet []byte // Stream packet
var MaxChunkSize uint64 = 64786

func (s stream_packet) get_id() []byte {
	return s[:4]
}
func (s stream_packet) get_seq() uint16 {
	return binary.BigEndian.Uint16(s[4:6])
}
func (s stream_packet) get_data() []byte {
	return s[6:]
}

func (s stream_packet) request_id() uint32 {
	return binary.BigEndian.Uint32(s[:4])
}

func (s stream_packet) isPayload() bool {
	return s.get_seq() == 1 && len(s.get_data()) == 8
}

func (s stream_packet) process(c *Conn) {
	id := decodeId(s.get_id())
	pending := c.pending_request.peek(id)

	if pending == nil {
		log.Println("[ERROR] " + ErrUnknownPendingRequest.Error())
		return
	}

	if pending.IsDone() {
		return
	}

	if pending.write(s) {
		c.pending_request.pop(id)
		pending.done()
		if pending.Error() == ErrUnexpectedStreamPacket {
			c.Close()
			// c.sendJSON(&response{ID: id, SV: "CANCEL", KW: nil})
		}
	}
}

func newStream(id []byte, seq uint16, data []byte) stream_packet {
	var s stream_packet = make([]byte, len(data)+6)
	copy(s, id[:4])
	binary.BigEndian.PutUint16(s[4:6], seq)
	copy(s[6:], data)

	return s
}

func endOfStream(id []byte, seq uint16) stream_packet {
	return newStream(id, seq, []byte(""))
}

func newPayload(id []byte, v uint64) stream_packet {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, v)

	return newStream(id, 1, data)
}

func decodePayload(data []byte) uint64 {
	return binary.BigEndian.Uint64(data)
}

func decodeId(data []byte) uint32 {
	return binary.BigEndian.Uint32(data)
}

func toStreamId(id uint32) []byte {
	id_b := make([]byte, 4)
	binary.BigEndian.PutUint32(id_b, id)
	return id_b
}

// Binary stream codec

func stream_marshal(v interface{}) (msg []byte, payloadType byte, err error) {
	return v.(stream_packet), websocket.BinaryFrame, nil
}

func stream_unmarshal(msg []byte, payloadType byte, v interface{}) error {
	b, ok := v.(*stream_packet)
	if !ok {
		return errors.New("Interface is not of the right type. Expected *stream_packet.")
		// Usage:
		//  var s stream_packet = make(stream_packet, 0)
		//  STREAM.Receive(ws, &s)
	}
	*b = msg

	return nil
}

var STREAM = websocket.Codec{stream_marshal, stream_unmarshal}

func message_marshal(v interface{}) (msg []byte, payloadType byte, err error) {

	switch data := v.(type) {

	case stream_packet:
		return data, websocket.BinaryFrame, nil

	default:
		msg, err = json.Marshal(v)
		return []byte(msg), websocket.TextFrame, nil

	}

}

func message_unmarshal(msg []byte, payloadType byte, v interface{}) (err error) {

	switch payloadType {

	case websocket.BinaryFrame:
		*v.(*message) = stream_packet(msg)

	case websocket.TextFrame:
		s := new(request)
		err = json.Unmarshal(msg, s)
		if err != nil {
			return
		}
		*v.(*message) = s

	}

	return
}

var MESSAGE = websocket.Codec{message_marshal, message_unmarshal}
