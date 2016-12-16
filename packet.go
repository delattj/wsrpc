package wsrpc

import (
	"log"
	"errors"
	"reflect"
	"strings"
	"encoding/json"
	"encoding/binary"
	"golang.org/x/net/websocket"
)

type Header struct {
	data map[string]interface{}
}

func newHeader() *Header {
	return &Header{make(map[string]interface{})}
}

func (h *Header) Has(key string) bool {
	_, ok := h.data[key]
	return ok
}
func (h *Header) Get(key string) interface{} {
	return h.data[key]
}
func (h *Header) Set(key string, data interface{}) {
	h.data[key] = data
}
func (h *Header) Delete(key string) {
	delete(h.data, key)
}
func (h *Header) Clone() *Header {
	n := newHeader()
	for k, d := range h.data {
		n.data[k] = d
	}
	return n
}
func (h *Header) writeTo(ws *websocket.Conn) error {
	return websocket.JSON.Send(ws, &h.data)
}
func (h *Header) readFrom(ws *websocket.Conn) error {
	if websocket.JSON.Receive(ws, &h.data) != nil { return ErrHandshake }
	if h.Has("error") {
		return errors.New(h.Get("error").(string))
	}
	return nil
}

type message interface { // Hold request or stream packet incoming message type
	process(*Conn)
}

type response struct { // When sending
	ID uint32
	SV string
	KW interface{}
}

type request struct { // When receiving
	ID uint32          // ID:0 means no return value
	SV string          // SV:"R" => Response, SV:"ERR" => Error report, the rest is a request
	KW json.RawMessage // to decode later depending of SV
}

func (msg *request) process(c *Conn) {
	switch msg.SV {
	case "ERR":
		fallthrough
	case "R":
		err := msg.handleResponse(c)
		if err != nil {
			log.Println("[ERROR] "+ err.Error())
		}

	case "CANCEL":
		// Cancel Stream
		msg.handleCancel(c)

	default:
		err := msg.handleRequest(c)
		if err != nil {
			if msg.ID != 0 {
				go c.sendResponse(msg.ID, nil, err)

			} else {
				log.Println("[ERROR] "+ err.Error())
			}
		}
	}
}

func (resp *request) handleResponse(c *Conn) (err error) {
	pending := c.pending_request.pop(resp.ID)
	if pending == nil {
		return ErrUnknownPendingRequest
	}

	// Decode Response content(KW) depending of the SV type (error or reply data)
	if resp.SV == "ERR" {
		var errMsg string
		err = json.Unmarshal(resp.KW, &errMsg)
		if err == nil {
			pending.setError(errors.New(errMsg))
		} else {
			pending.setError(err)
		}
		return
	}
	
	err = json.Unmarshal(resp.KW, pending.Reply())
	if err != nil {
		pending.setError(err)
		return
	}

	pending.done()
	return
}

func (req *request) handleCancel(c *Conn) {
	sender := c.streaming.pop(req.ID)
	if sender != nil {
		sender.setError(ErrCanceled)
	}
}

func (req *request) handleRequest(c *Conn) (err error) {

	methodName := req.SV
	dot := strings.LastIndex(methodName, ".")
	if dot < 0 {
		err = errors.New("wsrpc: service/method request ill-formed: "+ methodName)
		return
	}

	// Look up the request.
	s := c.srv
	if s == nil {
		err = ErrNoServices
		return
	}
	mtype := s.method[methodName]
	if mtype == nil {
		err = errors.New("wsrpc: can't find method "+ methodName)
		return
	}

	// Create the argument value.
	var argv, replyv reflect.Value
	argIsValue := false // if true, need to indirect before calling.
	if mtype.ArgType.Kind() == reflect.Ptr {
		argv = reflect.New(mtype.ArgType.Elem())
	} else {
		argv = reflect.New(mtype.ArgType)
		argIsValue = true
	}

	// Decode Request content(KW) depending of the Service (SV) now that an
	// empty argv was created to receive the data.
	err = json.Unmarshal(req.KW, argv.Interface())
	if err != nil {
		return
	}
	if argIsValue {
		argv = argv.Elem()
	}

	stream := false
	if mtype.ReplyType == typeOfStream {
		sender := newStreamer(req.ID, c)
		c.streaming.set(req.ID, sender)
		replyv = reflect.ValueOf(sender)
		stream = true

	} else {
		replyv = reflect.New(mtype.ReplyType.Elem())
	}

	go call(c, req.ID, mtype, argv, replyv, stream)

	return
}

func call(c *Conn, id uint32, mtype *methodType, argv, replyv reflect.Value, stream bool) {
	// defer func () { // Trap panic ???
	// 	if r := recover(); r != nil {
	// 		err = errors.New(fmt.Sprintf("%s", r))
	// 	}
	// }()
	// Invoke the method, providing a new value for the reply.
	function := mtype.method.Func
	returnValues := function.Call([]reflect.Value{c.srv.v_rcvr, c.value, argv, replyv})
	// The return value for the method is an error.
	errInter := returnValues[0].Interface()
	var err error
	if errInter != nil {
		err = errInter.(error)
	}

	if id != 0 {
		if !stream {
			c.sendResponse(id, replyv.Interface(), err)

		} else if err != nil {
			// Send only error to stream
			// But first check if the stream was completed
			stream := replyv.Interface().(StreamSender)
			if !stream.IsDone() {
				stream.setError(err)
			}
			c.sendResponse(id, nil, err)
		}

	} else if err != nil {
		log.Println("[ERROR] "+ err.Error())
	}
}

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
		log.Println("[ERROR] "+ ErrUnknownPendingRequest.Error())
		return
	}

	if pending.IsDone() { return }

	if pending.write(s) {
		c.pending_request.pop(id)
		pending.done()
	}
}

func newStream(id []byte, seq uint16, data []byte) stream_packet {
	var s stream_packet = make([]byte, len(data) + 6)
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

