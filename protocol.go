package wsrpc

import (
	"log"
	"io"
	// "fmt"
	"time"
	"net/http"
	"golang.org/x/net/websocket"
	"encoding/binary"
	"encoding/json"
	"crypto/rand"
	"reflect"
	"sync"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

type request struct { // When receiving
	ID uint32          // ID:0 means no return value
	SV string          // SV:"R" => Response, SV:"ERR" => Error report, the rest is a request
	KW json.RawMessage // to decode later depending of SV
}

type response struct { // When sending
	ID uint32
	SV string
	KW interface{}
}

type stream_packet []byte // Stream packet
var MaxChunkSize uint64 = 64787

type message interface { // Hold request or stream packet incoming message type
	process(*Conn)
}

// Precompute the reflect type for error.  Can't use error directly
// because Typeof takes an empty interface value.  This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()
var typeOfStream = reflect.TypeOf((*StreamSender)(nil)).Elem()
var typeOfConn = reflect.TypeOf((*Conn)(nil)).Elem()

var (
	ErrTimeout					= errors.New("wsrpc: Task timeout.")
	ErrCanceledLostConn 		= errors.New("wsrpc: Canceled task. Lost remote connection.")
	ErrCanceled					= errors.New("wsrpc: Canceled task.")
	ErrUnknownPendingRequest	= errors.New("wsrpc: Unknown pending request id.")
	ErrNoServices				= errors.New("wsrpc: No registered services.")
	ErrFileAlreadyStreamed		= errors.New("wsrpc: A file was already streamed through this request id.")
	ErrDataAlreadyStreamed		= errors.New("wsrpc: Data was already streamed through this request id.")
	ErrCloseChan 				= errors.New("send on closed channel")
	ErrConnectionClosed			= errors.New("wsrpc: Closed connections.")
	ErrUnexpectedStreamPacket	= errors.New("wsrpc: Did not expect a stream packet for this pending request.")
	ErrNoMoreData				= errors.New("wsrpc: No more data to read from buffer io.")
)

// Type use to as place of arguments and/or reply type in method signature when
// argument or reply is unnecessary.
type Nothing *bool
// Variable use to signify that no response is necessary to be sent back.
// In essence it's just nil.
var NoResponse Nothing

type methodType struct {
	method     reflect.Method
	ArgType    reflect.Type
	ReplyType  reflect.Type
}

type Service interface {
	OnConnect(c *Conn)
	OnDisconnect(c *Conn)
}

type service struct {
	rcvr    Service                // receiver of methods
	v_rcvr  reflect.Value          // Value of the receiver
	typ     reflect.Type           // type of the receiver
	method  map[string]*methodType // registered methods
}

func (c *Conn) serve() {
	// handling Connect/Disconnect events
	c.connected()
	if c.srv.rcvr != nil {
		go c.srv.rcvr.OnConnect(c)
		defer c.srv.rcvr.OnDisconnect(c)
	}
	defer c.disconnected()

	for { // message dispatch loop
		m, err := c.readMessage()
		if err != nil {
			if err == io.EOF {
				log.Println("[WARNING] Connection closed.")

			} else {
				log.Println("[ERROR] "+ err.Error())
			}
			break
		}

		if m != nil {
			m.process(c)
		}
	}
}

func (s *service) register(rcvr Service) error {
	s.rcvr = rcvr
	s.typ = reflect.TypeOf(rcvr)
	s.v_rcvr = reflect.ValueOf(rcvr)
	sname := reflect.Indirect(s.v_rcvr).Type().Name()
	if sname == "" {
		er := "wsrpc register: no name for type " + s.typ.String()
		log.Println("[ERROR]", er)
		return errors.New(er)
	}
	if !isExported(sname) {
		er := "wsrpc register: type " + sname + " is not exported"
		log.Println("[ERROR]", er)
		return errors.New(er)
	}

	// Install the methods
	s.method = suitableMethods(s.typ, sname, true)

	if len(s.method) == 0 {
		str := "wsrpc register: type "+ sname +" has no exported methods of suitable type"

		// To help the user, see if a pointer receiver would work.
		method := suitableMethods(reflect.PtrTo(s.typ), "", false)
		if len(method) != 0 {
			str = str +" (hint: pass a pointer to value of that type)"
		}
		log.Println("[ERROR]", str)
		return errors.New(str)
	}
	return nil
}

// suitableMethods returns suitable Rpc methods of typ, it will report
// error using log if reportErr is true.
func suitableMethods(typ reflect.Type, sname string, reportErr bool) map[string]*methodType {
	methods := make(map[string]*methodType)
	for m := 0; m < typ.NumMethod(); m++ {
		method := typ.Method(m)
		mtype := method.Type
		mname := sname +"."+ method.Name
		// Discard OnConnect/OnDisconnect events
		if method.Name == "OnConnect" || method.Name == "OnDisconnect" {
			continue
		}
		// Method must be exported.
		if method.PkgPath != "" {
			continue
		}
		// Method needs three ins: receiver, *Conn, *args, *reply.
		if mtype.NumIn() != 4 {
			if reportErr {
				log.Println("[ERROR] method", mname, "has wrong number of ins:", mtype.NumIn())
			}
			continue
		}
		// First arg need not be a pointer.
		if connType := mtype.In(1); connType.Kind() != reflect.Ptr || connType.Elem() != typeOfConn {
			if reportErr {
				log.Println("[ERROR]", mname, "first argument must be *wsrpc.Conn type, not:", connType)
			}
			continue
		}
		// Second arg need not be a pointer.
		argType := mtype.In(2)
		if !isExportedOrBuiltinType(argType) {
			if reportErr {
				log.Println("[ERROR]", mname, "argument type not exported:", argType)
			}
			continue
		}
		// Third arg must be a pointer.
		replyType := mtype.In(3)
		if replyType.Kind() != reflect.Ptr && replyType != typeOfStream {
			if reportErr {
				log.Println("[ERROR] method", mname, "reply type not a pointer:", replyType)
			}
			continue
		}
		// Reply type must be exported.
		if !isExportedOrBuiltinType(replyType) {
			if reportErr {
				log.Println("[ERROR] method", mname, "reply type not exported:", replyType)
			}
			continue
		}
		// Method needs one out.
		if mtype.NumOut() != 1 {
			if reportErr {
				log.Println("[ERROR] method", mname, "has wrong number of outs:", mtype.NumOut())
			}
			continue
		}
		// The return type of the method must be error.
		if returnType := mtype.Out(0); returnType != typeOfError {
			if reportErr {
				log.Println("[ERROR] method", mname, "returns", returnType.String(), "not error")
			}
			continue
		}
		methods[mname] = &methodType{method: method, ArgType: argType, ReplyType: replyType}
	}
	return methods
}

// Is this an exported - upper case - name?
func isExported(name string) bool {
	rune, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(rune)
}

// Is this type exported or a builtin?
func isExportedOrBuiltinType(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return isExported(t.Name()) || t.PkgPath() == ""
}

// PendingRequest is used to know when a remote call has finished and reply
// value is ready to read.
type PendingRequest interface {
	Reply() interface{}
	OnDone() chan bool // Event channel. Can be used with select statement
	IsDone() bool
	Error() error
	Wait() error
	WaitTimeout(time.Duration) error
	HasFailed() bool
	done()
	setError(error)
	write(stream_packet) bool
}
// Use p.Wait(), p.WaitTimeout(t) or <-p.OnDone() to know when the reply value is ready.
// p.Wait() and p.WaitTimeout(t) return remote error or wsrpc.ErrTimeout.
// Reply value and Error message are retrivable respectively through p.Reply() and p.Error()
// You should access those fields only when p.OnDone() is released.

type pendingRequest struct {
	reply interface{}
	ondone chan bool
	error error
}

func newPendingRequest(reply interface{}) PendingRequest {
	return &pendingRequest{reply:reply, ondone:make(chan bool)}
}

func (p *pendingRequest) done() {
	close(p.ondone)
}

func (p *pendingRequest) write(s stream_packet) bool {
	panic(ErrUnexpectedStreamPacket)
	return false
}

func (p *pendingRequest) OnDone() chan bool {
	return p.ondone
}

func (p *pendingRequest) Reply() interface{} {
	return p.reply
}

func (p *pendingRequest) Error() error {
	return p.error
}

func (p *pendingRequest) setError(err error) {
	p.error = err
	p.done()
}

func (p *pendingRequest) Wait() error {
	<-p.ondone
	return p.error
}

// Like Wait() but return wsrpc.ErrTimeout if timeout is reached
func (p *pendingRequest) WaitTimeout(t time.Duration) error {
	ticker := time.NewTicker(t)
	defer ticker.Stop()
	
	select {
	case <-p.ondone:
		return p.error

	case <-ticker.C:
		return ErrTimeout
	}
}

func (p *pendingRequest) IsDone() bool {
	select {
	case <-p.ondone:
		return true
	default:
		return false
	}
}

func (p *pendingRequest) HasFailed() bool { return p.error != nil }

// Request id generator

type requestId struct {
	n uint32
	lock sync.Mutex
}

func (rid *requestId) get() uint32 {
	rid.lock.Lock()

	rid.n++
	if rid.n == 0 {
		rid.n = 1
	}

	rid.lock.Unlock()

	return rid.n
}

// Main connection struct.
type Conn struct {
	ws *websocket.Conn
	srv *service
	request_id *requestId
	pending_request *pendingMap
	stream_channel chan stream_packet
	streaming *streamingMap
	ondisconnect chan bool // Event channel. Can be used with select statement
	value reflect.Value
}
// Use c.IsConnected() or <-c.OnDisconnect() to know when connection is closed.

type streamingMap struct {
	lock sync.Mutex
	book map[uint32]StreamSender
}

func (s *streamingMap) set(id uint32, item StreamSender) {
	s.lock.Lock()
	s.book[id] = item
	s.lock.Unlock()
}

func (s *streamingMap) pop(id uint32) (item StreamSender) {
	s.lock.Lock()
	item = s.book[id]
	if item != nil {
		delete(s.book, id)
	}
	s.lock.Unlock()
	return
}

func (s *streamingMap) dropAll(err error) {
	s.lock.Lock()
	for id, item := range s.book {
		item.setError(err)
		delete(s.book, id)
	}
	s.lock.Unlock()
}

func newStreamingMap() *streamingMap {
	return &streamingMap{book:make(map[uint32]StreamSender)}
}

type pendingMap struct {
	lock sync.RWMutex
	book map[uint32]PendingRequest
}

func (s *pendingMap) set(id uint32, item PendingRequest) {
	s.lock.Lock()
	s.book[id] = item
	s.lock.Unlock()
}

func (s *pendingMap) pop(id uint32) (item PendingRequest) {
	s.lock.Lock()
	item = s.book[id]
	if item != nil {
		delete(s.book, id)
	}
	s.lock.Unlock()
	return
}

func (s *pendingMap) peek(id uint32) (item PendingRequest) {
	s.lock.RLock()
	item = s.book[id]
	s.lock.RUnlock()
	return
}

func (s *pendingMap) dropAll(err error) {
	s.lock.Lock()
	for id, item := range s.book {
		item.setError(err)
		delete(s.book, id)
	}
	s.lock.Unlock()
}

func newPendingMap() *pendingMap {
	return &pendingMap{book:make(map[uint32]PendingRequest)}
}

func newConn(ws *websocket.Conn, srv *service) *Conn {
	ws.PayloadType = websocket.BinaryFrame // Default to binary frame for stream
	c := Conn{ws:ws, srv:srv, request_id:&requestId{},
				pending_request:newPendingMap(), streaming:newStreamingMap()}
	c.value = reflect.ValueOf(&c)
	return &c
}

func (c *Conn) connected() {
	c.ondisconnect = make(chan bool)
	c.stream_channel = make(chan stream_packet)
	go c.streamSender()
}

func (c *Conn) IsConnected() bool {
	select {
	case <-c.ondisconnect:
		return false
	default:
		return true
	}
}

func (c *Conn) OnDisconnect() chan bool {
	return c.ondisconnect
}

func (c *Conn) disconnected() {
	close(c.ondisconnect)
	close(c.stream_channel)
	c.pending_request.dropAll(ErrCanceledLostConn)
	c.streaming.dropAll(ErrCanceledLostConn)
}

func (c *Conn) Close() error { return c.ws.Close() }
func (c *Conn) Request() *http.Request { return c.ws.Request() }
func (c *Conn) SetDeadline(t time.Time) error { return c.ws.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error { return c.ws.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }


// Send a remote call to the connected service. Return a PendingRequest if successful.
// Sending the request is done synchronously. Waiting for the result is not.
func (c *Conn) RemoteCall(name string, kwargs, reply interface{}) (pending PendingRequest, err error) {
	need_response := (reply != nil)

	var id uint32 = 0
	if need_response {
		id = c.request_id.get()
	}
	r := response{ID:id, SV:name, KW:kwargs}

	err = websocket.JSON.Send(c.ws, &r)

	if err != nil {
		return
	}

	if !need_response {
		return
	}

	pending = newPendingRequest(reply)
	c.pending_request.set(id, pending)

	return
}

func (c *Conn) readMessage() (m message, err error) {
	err = MESSAGE.Receive(c.ws, &m)
	return
}

func (c *Conn) sendResponse(id uint32, replyv interface{}, err error) {
	r_type := "R"
	if err != nil  {
		replyv = "(remote) "+ err.Error()
		r_type = "ERR"
	}

	r := response{ID: id, SV: r_type, KW: replyv}

	err = websocket.JSON.Send(c.ws, &r)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
	}
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

func (c *Conn) streamSender() {
	for s := range c.stream_channel {
		c.ws.Write(s)
	}
}

// Equivalent to RemoteCall(), but expect a binary stream response.
// Reply destination (dst) must io.Writer.
func (c *Conn) RemoteStream(name string, kwargs interface{}) (pending StreamReceiver, err error) {
	id := c.request_id.get()

	r := response{ID:id, SV:name, KW:kwargs}

	err = websocket.JSON.Send(c.ws, &r)
	if err != nil { return }

	pending = newPendingWriter(id, c)
	c.pending_request.set(id, pending)

	return
}

// Stream sender

type StreamSender interface {
	Send([]byte) error
	SendFile(io.Reader, uint64) error
	End()
	Cancel() error
	Progress() (uint64, uint64)
	Error() error
	Wait() error
	WaitTimeout(time.Duration) error
	HasFailed() bool
	OnDone() chan bool
	IsDone() bool
	setError(error)
}

type streamer struct {
	c *Conn
	id uint32
	id_b []byte
	seq uint8
	packet_total uint64
	packet_sent uint64
	ondone chan bool
	error error
	progress sync.RWMutex
}

func newStreamer(id uint32, c *Conn) StreamSender {
	return &streamer{c:c, id:id, id_b:toStreamId(id), ondone:make(chan bool)}
}

// Calculate number of chunk
func PacketRequire(length uint64) uint64 {
	n := length / MaxChunkSize
	if length % MaxChunkSize > 0 {
		n++
	}
	return n
}

func recoverFromCloseChannel() bool {
	if recover() != nil { return true }
	return false
}

func (s *streamer) Send(data []byte) error {
	s.progress.Lock()
	if s.packet_total != 0 {
		s.progress.Unlock()
		return ErrFileAlreadyStreamed
	}
	s.seq++
	seq := s.seq
	s.progress.Unlock()

	if s.sendPacket(newStream(s.id_b, seq, data)) { return nil }

	return s.error
}

func (s *streamer) End() {
	s.progress.Lock()
	if s.packet_total != 0 {
		s.progress.Unlock()
		panic(ErrFileAlreadyStreamed)
	}
	s.seq++
	seq := s.seq
	s.progress.Unlock()

	if s.sendPacket(endOfStream(s.id_b, seq)) { s.done() }
}

func(s *streamer) sendPacket(packet stream_packet) (result bool) {
	defer recoverFromCloseChannel()
	select {
	case s.c.stream_channel <-packet:
		return true

	case <-s.ondone:
		return false
	}
}

func (s *streamer) SendFile(r io.Reader, length uint64) (err error) {
	s.progress.Lock()
	if s.packet_total != 0 {
		s.progress.Unlock()
		return ErrFileAlreadyStreamed
	} else if s.seq != 0 {
		s.progress.Unlock()
		return ErrDataAlreadyStreamed
	}
	n := PacketRequire(length) // Calculate number of chunk
	s.packet_total = n
	s.progress.Unlock()
	// Convert id
	id_b := s.id_b
	b := make([]byte, MaxChunkSize)
	// Payload first
	if !s.sendPacket(newPayload(id_b, length)) {
		return s.error
	}
	// Loop through all chunks
	var i uint64
	var seq uint8 = 1
	for i = 0; i < n; i++ {
		seq++

		if length < MaxChunkSize {
			b = b[:length]
		}

		nr, _ := r.Read(b)
		if nr == 0 {
			return ErrNoMoreData
		}

		if !s.sendPacket(newStream(id_b, seq, b[:nr])) {
			return s.error
		}

		length -= uint64(nr)
		s.progress.Lock()
		s.packet_sent++
		s.progress.Unlock()
	}

	// Send end of feed
	seq++
	if !s.sendPacket(endOfStream(id_b, seq)) {
		return s.error
	}

	s.done()

	return nil
}

func (s *streamer) Cancel() error {
	r := response{ID: s.id, SV: "ERR", KW: ErrCanceled.Error()}

	s.setError(ErrCanceled)

	return websocket.JSON.Send(s.c.ws, &r)
}

func (s *streamer) setError(err error) {
	s.error = err
	s.done()
}

func (s * streamer) Progress() (uint64, uint64) {
	s.progress.RLock()
	defer s.progress.RUnlock()
	return s.packet_sent, s.packet_total
}

func (s *streamer) done() {
	s.c.streaming.pop(s.id)
	close(s.ondone)
}

func (s *streamer) Error() error {
	return s.error
}

func (s *streamer) HasFailed() bool { return s.error != nil }

func (s *streamer) OnDone() chan bool {
	return s.ondone
}

func (s *streamer) Wait() error {
	<-s.ondone
	return s.error
}

func (s *streamer) WaitTimeout(t time.Duration) error {
	ticker := time.NewTicker(t)
	defer ticker.Stop()
	
	select {
	case <-s.ondone:
		return s.error

	case <-ticker.C:
		return ErrTimeout
	}
}

func (s *streamer) IsDone() bool {
	select {
	case <-s.ondone:
		return true
	default:
		return false
	}
}

// Pending Writer

type StreamReceiver interface {
	Reply() interface{}
	OnDone() chan bool // Event channel. Can be used with select statement
	IsDone() bool
	Error() error
	Wait() error
	WaitTimeout(time.Duration) error
	HasFailed() bool
	done()
	setError(error)
	write(stream_packet) bool

	Cancel() error
	Progress() (uint64, uint64)
	Receive() ([]byte, error)
	ReceiveFile(io.Writer) error
}

type pendingWriter struct {
	pendingRequest
	id uint32
	c *Conn
	queue chan []byte
	seq_buffer map[uint8][]byte
	seq uint8
	writing sync.Mutex
	progress sync.RWMutex
	packet_total uint64
	packet_sent uint64
}

func newPendingWriter(id uint32, c *Conn) StreamReceiver {
	return &pendingWriter{
		pendingRequest:pendingRequest{ondone:make(chan bool)},
		id:id, c:c, queue:make(chan []byte, 256), seq_buffer:make(map[uint8][]byte),
	}
}

func (p *pendingWriter) write(s stream_packet) (done bool) {
	p.writing.Lock()
	defer p.writing.Unlock()

	seq := s.get_seq()
	data := s.get_data()
	next_seq := p.seq + 1
	if next_seq == seq {
		if len(data) == 0 {
			// Empty data means end of feed
			return true
		}
		p.queue <- data
		p.seq, done = p.write_buffer(next_seq)

	} else {
		p.seq_buffer[seq] = data
	}
	return
}

func (p *pendingWriter) write_buffer(seq uint8) (uint8, bool) {
	if len(p.seq_buffer) > 0 {
	loop:
		next_seq := seq + 1
		data, ok := p.seq_buffer[next_seq]
		if ok {
			seq = next_seq
			if len(data) > 0 {
				p.queue <- data
				delete(p.seq_buffer, next_seq)
				goto loop
			}
			// Empty data means end of feed
			return seq, true
		}
	}
	return seq, false
}

func (p *pendingWriter) done() {
	p.pendingRequest.done()
	p.writing.Lock()
	close(p.queue)
	p.writing.Unlock()
}

func (p *pendingWriter) setError(err error) {
	p.error = err
	p.done()
}

// Empty data and error == nil means closed stream.
func (p *pendingWriter) Receive() ([]byte, error) {
	data := <- p.queue
	return data, p.error
}

func (p *pendingWriter) ReceiveFile(dst io.Writer) error {
	// Very first packet is payload
	data := <- p.queue
	if data == nil { return p.error }
	if len(data) != 8 {
		err := errors.New("wsrpc: Wrong bytes size for payload.")
		p.setError(err)
		return err

	}
	p.setPacketRequire(decodePayload(data))

	for data := range p.queue {
		dst.Write(data)
		p.incrementPacketSent()
	}

	return p.error
}

func (p *pendingWriter) setPacketRequire(l uint64) {
	p.progress.Lock()
	p.packet_total = PacketRequire(l)
	p.progress.Unlock()
}

func (p *pendingWriter) incrementPacketSent() {
	p.progress.Lock()
	p.packet_sent++
	p.progress.Unlock()
}

func (p *pendingWriter) Progress() (uint64, uint64) {
	p.progress.RLock()
	defer p.progress.RUnlock()
	return p.packet_sent, p.packet_total
}

func (p *pendingWriter) Cancel() error {
	r := response{ID: p.id, SV: "CANCEL", KW: nil}

	p.c.pending_request.pop(p.id)
	p.setError(ErrCanceled)

	return websocket.JSON.Send(p.c.ws, &r)
}

// Stream packet (implement message interface)

func (s stream_packet) get_id() []byte {
	return s[:4]
}
func (s stream_packet) get_seq() uint8 {
	return s[4]
}
func (s stream_packet) get_data() []byte {
	return s[5:]
}

func (s stream_packet) request_id() uint32 {
	return binary.BigEndian.Uint32(s[:4])
}

func (s stream_packet) isPayload() bool {
	return s.get_seq() == 0 && len(s.get_data()) == 8
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

func newStream(id []byte, seq uint8, data []byte) stream_packet {
	var s stream_packet = make([]byte, len(data) + 5)
	copy(s, id[:4])
	s[4] = seq
	copy(s[5:], data)

	return s
}

func endOfStream(id []byte, seq uint8) stream_packet {
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

func GenerateStreamId() []byte {
	b := make([]byte, 4)
	rand.Read(b)
	return b
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



