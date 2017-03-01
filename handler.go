package wsrpc

import (
	"encoding/json"
	"errors"
	"golang.org/x/net/websocket"
	"io"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"
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
	if websocket.JSON.Receive(ws, &h.data) != nil {
		return ErrHandshake
	}
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
			log.Println("[ERROR] " + err.Error())
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
				log.Println("[ERROR] " + err.Error())
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

	r := pending.Reply()
	if r == nil {
		pending.setError(ErrUnexpectedJSONPacket) // Panic?Disconnect?
		return
	}
	err = json.Unmarshal(resp.KW, r)
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
		err = errors.New("wsrpc: service/method request ill-formed: " + methodName)
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
		err = errors.New("wsrpc: can't find method " + methodName)
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
		log.Println("[ERROR] " + err.Error())
	}
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
	reply  interface{}
	ondone chan bool
	error  error
}

func newPendingRequest(reply interface{}) PendingRequest {
	return &pendingRequest{reply: reply, ondone: make(chan bool)}
}

func (p *pendingRequest) done() {
	close(p.ondone)
}

func (p *pendingRequest) write(s stream_packet) bool {
	p.error = ErrUnexpectedStreamPacket // Panic?Disconnect?
	return true
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
	c            *Conn
	id           uint32
	id_b         []byte
	seq          uint16
	packet_total uint64
	packet_sent  uint64
	ondone       chan bool
	error        error
	progress     sync.RWMutex
}

func newStreamer(id uint32, c *Conn) StreamSender {
	return &streamer{c: c, id: id, id_b: toStreamId(id), ondone: make(chan bool)}
}

// Calculate number of chunk
func PacketRequire(length uint64) uint64 {
	n := length / MaxChunkSize
	if length%MaxChunkSize > 0 {
		n++
	}
	return n
}

// func recoverFromCloseChannel() bool {
// 	if recover() != nil { return true }
// 	return false
// }

func (s *streamer) Send(data []byte) error {
	s.progress.Lock()
	if s.packet_total != 0 {
		s.progress.Unlock()
		return ErrFileAlreadyStreamed
	}
	s.seq++
	seq := s.seq
	s.progress.Unlock()

	return s.sendPacket(newStream(s.id_b, seq, data))
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

	if s.sendPacket(endOfStream(s.id_b, seq)) == nil {
		s.done()
	}
}

func (s *streamer) sendPacket(packet stream_packet) error {
	if s.IsDone() {
		return s.error
	}
	// return s.c.sendByte(packet) // One Connection
	// Multiple Connections
	ws, err := s.c.pool.Get()
	if err != nil {
		return err
	}
	ws.sendByte(packet)
	return nil
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
	err = s.sendPacket(newPayload(id_b, length))
	if err != nil {
		return
	}
	// Loop through all chunks
	var i uint64
	var seq uint16 = 1
	for i = 0; i < n; i++ {
		seq++

		if length < MaxChunkSize {
			b = b[:length]
		}

		nr, _ := r.Read(b)
		if nr == 0 {
			return ErrNoMoreData
		}

		err = s.sendPacket(newStream(id_b, seq, b[:nr]))
		if err != nil {
			return
		}

		length -= uint64(nr)
		s.progress.Lock()
		s.packet_sent++
		s.progress.Unlock()
	}

	// Send end of feed
	seq++
	err = s.sendPacket(endOfStream(id_b, seq))
	if err != nil {
		return
	}

	s.done()

	return nil
}

func (s *streamer) Cancel() error {
	r := response{ID: s.id, SV: "ERR", KW: ErrCanceled.Error()}

	s.setError(ErrCanceled)

	return s.c.sendJSON(&r)
}

func (s *streamer) setError(err error) {
	s.error = err
	s.done()
}

func (s *streamer) Progress() (uint64, uint64) {
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

type bytePacket struct {
	data []byte
	next *bytePacket
}

type byteList struct {
	head *bytePacket
	tail *bytePacket
}

func (l *byteList) Append(data []byte) {
	new_p := &bytePacket{data, nil}
	if l.tail != nil {
		l.tail.next = new_p
	} else {
		l.head = new_p
	}
	l.tail = new_p
}

func (l *byteList) Prepend(data []byte) {
	new_p := &bytePacket{data, l.head}
	if l.head == nil {
		l.tail = new_p
	}
	l.head = new_p
}

func (l *byteList) Extend(list *byteList) {
	new_p := list.head
	if l.tail != nil {
		l.tail.next = new_p
	} else {
		l.head = new_p
	}
	l.tail = list.tail
}

func (l *byteList) Pop() []byte {
	first_p := l.head
	if first_p == nil {
		return nil
	}
	l.head = first_p.next
	if l.head == nil {
		l.tail = nil
	}
	return first_p.data
}

func (l *byteList) IsEmpty() bool {
	return l.head == nil
}

type sequence struct {
	first uint16
	last  uint16
	list  *byteList
	next  *sequence
}

func newSequence(seq uint16, data []byte) *sequence {
	b := &sequence{seq, seq, new(byteList), nil}
	b.list.Append(data)
	return b
}

func insertInSequence(d *sequence, seq uint16, data []byte, cursor uint16) (top *sequence) {
	if d == nil {
		return newSequence(seq, data)
	}
	top = d
	n := seq - 1
	e := seq + 1
	next := d.next
	if seq < cursor && top.first > cursor {
		// seq loop through uint16
		// search for the sequence that made the jump from max to min uint16
		for next != nil && next.last > cursor {
			d = next
			next = d.next
		}
		if next != nil && d.last > cursor {
			// seq is right between d.last and next.first
			if next.first < cursor && seq < next.first {
				goto insert
			}
			// If not, we can skip the sequence and start searching
			d = next
			next = d.next
		}
	} else if seq < top.first {
		// seq is before top sequence
		if top.first == e {
			top.list.Prepend(data)
			top.first = seq
		} else {
			top = newSequence(seq, data)
			top.next = d
		}
		return
	}
	// Search the sequences between which seq will be inserted
	for next != nil {
		if seq > d.last && (seq < next.first || (next.first < cursor && seq > cursor)) {
			break
		}
		d = next
		next = d.next
	}
insert:
	if d.last == n {
		d.list.Append(data)
		if next != nil && next.first == e {
			// We extend the list in which we already append data,
			// Let's merge it to form a continuous sequence.
			d.list.Extend(next.list)
			d.next = next.next
			d.last = next.last
		} else {
			d.last = seq
		}
		return
	}
	if next != nil && next.first == e {
		next.list.Prepend(data)
		next.first = seq
		return
	}
	// no prepend, no append occured,
	// we insert a new sequence
	// d.last < seq < next.first
	d.next = newSequence(seq, data)
	d.next.next = next
	return
}

type pendingWriter struct {
	pendingRequest
	id           uint32
	c            *Conn
	queue        *byteList
	towrite      chan bool
	seq_cache    *sequence
	seq          uint16
	writing      sync.Mutex
	progress     sync.RWMutex
	packet_total uint64
	packet_sent  uint64
}

func newPendingWriter(id uint32, c *Conn) StreamReceiver {
	return &pendingWriter{
		pendingRequest: pendingRequest{ondone: make(chan bool)},
		id:             id, c: c, towrite: make(chan bool, 1), queue: new(byteList),
	}
}

func (p *pendingWriter) write(s stream_packet) (end_of_feed bool) {
	p.writing.Lock()
	defer p.writing.Unlock()

	// Handle Async write (Multiple Connections)
	seq := s.get_seq()
	data := s.get_data()
	next_seq := p.seq + 1
	if next_seq == seq {
		p.notifyToWrite()
		if len(data) == 0 {
			// Empty data means end of feed
			return true
		}
		p.queue.Append(data)
		p.seq, end_of_feed = p.getBuffer(seq)

	} else {
		p.seq_cache = insertInSequence(p.seq_cache, seq, data, p.seq)
	}
	return
}

func (p *pendingWriter) notifyToWrite() {
	select { // Notify data is ready
	case p.towrite <- true:
	default:
	}
}

func (p *pendingWriter) getBuffer(seq uint16) (uint16, bool) {
	seq_cache := p.seq_cache
	if seq_cache != nil && seq_cache.first == (seq+1) {
		p.queue.Extend(seq_cache.list)
		p.seq_cache = seq_cache.next
		// Empty data means end of feed
		return seq_cache.last, len(seq_cache.list.tail.data) == 0
	}
	return seq, false
}

func (p *pendingWriter) done() {
	p.pendingRequest.done()
	p.writing.Lock()
	close(p.towrite)
	p.writing.Unlock()
}

func (p *pendingWriter) setError(err error) {
	p.error = err
	p.done()
}

// Empty data and error == nil means closed stream.
func (p *pendingWriter) Receive() ([]byte, error) {
	var data []byte
	if <-p.towrite || !p.queue.IsEmpty() {
		p.writing.Lock()
		data = p.queue.Pop()
		if !p.queue.IsEmpty() && !p.IsDone() {
			p.notifyToWrite()
		}
		p.writing.Unlock()
	}
	return data, p.error
}

func (p *pendingWriter) ReceiveFile(dst io.Writer) error {
	// Very first packet is payload
	data, err := p.Receive()
	if data == nil {
		return err
	}
	if len(data) != 8 {
		err = errors.New("wsrpc: Wrong bytes size for payload.")
		p.setError(err)
		return err

	}
	p.setPacketRequire(decodePayload(data))

	for <-p.towrite || !p.queue.IsEmpty() {
		p.writing.Lock()
		for data = p.queue.Pop(); data != nil; data = p.queue.Pop() {
			_, err = dst.Write(data)
			if err != nil {
				p.writing.Unlock()
				return err
			}
			p.incrementPacketSent()
		}
		p.writing.Unlock()
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

	return p.c.sendJSON(&r)
}
