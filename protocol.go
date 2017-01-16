package wsrpc

import (
	"io"
	"log"
	"net"
	"sync"
	"time"
	"errors"
	"reflect"
	"unicode"
	"crypto/rand"
	"unicode/utf8"
	"encoding/hex"
	"encoding/binary"
	"golang.org/x/net/websocket"
)

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
	ErrHandshake				= errors.New("wsrpc: Handshake failed.")
)

// Type use to as place of arguments and/or reply type in method signature when
// argument or reply is unnecessary.
type Nothing *bool
// Variable use to signify that no response is necessary to be sent back.
// In essence it's just nil.
var NoResponse Nothing

var ConnTimeout = 5 * time.Minute

type methodType struct {
	method     reflect.Method
	ArgType    reflect.Type
	ReplyType  reflect.Type
}

type Service interface {
	OnHandshake(*Header) error
	OnConnect(*Conn)
	OnDisconnect(*Conn)
}

type service struct {
	rcvr     Service                // receiver of methods
	v_rcvr   reflect.Value          // Value of the receiver
	typ      reflect.Type           // type of the receiver
	method   map[string]*methodType // registered methods
	mux      map[string]*Conn       // multiplexer connection
	lock     sync.Mutex
	max_conn uint8
}

func newService() (s *service) {
	return &service{mux:make(map[string]*Conn), max_conn:1}
}

func (srv *service) getMux(id string) *Conn {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	return srv.mux[id]
}

func (srv *service) putMux(id string, c *Conn) {
	srv.lock.Lock()
	srv.mux[id] = c
	srv.lock.Unlock()
}

func (srv *service) removeMux(id string) {
	srv.lock.Lock()
	delete(srv.mux, id)
	srv.lock.Unlock()
}

func (srv *service) maxSocket(max uint8) {
	srv.max_conn = max
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
		// Discard OnHandshake/OnConnect/OnDisconnect events
		if method.Name == "OnHandshake" || method.Name == "OnConnect" || method.Name == "OnDisconnect" {
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

// Request id generator
func generateId() (id uint32) {
	for id == 0 {
		b := make([]byte, 4)
		rand.Read(b)
		id = binary.BigEndian.Uint32(b)
	}
	return 
}
func generateByteId(length int) []byte {
	b := make([]byte, length)
	rand.Read(b)
	return  b
}
func generateHexId(length int) string {
	return hex.EncodeToString(generateByteId(length))
}

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
	for _, item := range s.book {
		item.setError(err)
	}
	s.book = make(map[uint32]StreamSender)
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
	for _, item := range s.book {
		item.setError(err)
	}
	s.book = make(map[uint32]PendingRequest)
	s.lock.Unlock()
}

func newPendingMap() *pendingMap {
	return &pendingMap{book:make(map[uint32]PendingRequest)}
}

// Wrap around the websocket.Conn struct
type wsConn struct {
	*websocket.Conn
	binary chan []byte
	ondisconnect chan bool
	header *Header
	id string
	tl_response time.Time
}

func (c *wsConn) isConnected() bool {
	select {
	case <-c.ondisconnect:
		return false
	default:
		return true
	}
}

func (c *wsConn) handshake(srv *service) (err error) {
	header := c.header
	if c.IsServerConn() {
		if err := header.readFrom(c.Conn); err != nil { return err }
		if !header.Has("channel") { return ErrHandshake }
		id := header.Get("channel").(string)
		if id == "" {
			// New connection, generate random id
			id = generateHexId(6)
		} else {
			// Validate existing id
			mux := srv.getMux(id)
			if mux == nil {
				err = errors.New("Invalid channel id")
			}
		}
		header.Set("channel", id)
		c.id = id

	} else if !header.Has("channel") { // If Client Conn has no id,
		header.Set("channel", "")      // request an id (send empty id)
	}
	// Custom handshake
	if err == nil && srv.rcvr != nil {
		err = srv.rcvr.OnHandshake(header)
	}
	if err != nil {
		header.Set("error", err.Error())
	}
	// Send header
	if header.writeTo(c.Conn) != nil { return ErrHandshake }
	if err != nil { return err }
	if c.IsClientConn() {
		if err := header.readFrom(c.Conn); err != nil { return err }
		if !header.Has("channel") { return ErrHandshake }
		c.id = header.Get("channel").(string)
	}

	return
}

func (c *wsConn) validate(srv *service) error {
	if err := c.handshake(srv); err != nil {
		c.Close()
		return err
	}
	return nil
}

func (c *wsConn) clean(mux *Conn) {
	close(c.ondisconnect)
	mux.pool.Prune(c, false)
}

func (c *wsConn) serve(mux *Conn) {
	defer c.clean(mux)
	go c.sender(mux)
	for { // message dispatch loop
		if c.IsServerConn() {
			c.SetDeadline(time.Now().Add(ConnTimeout))
		} else {
			c.tl_response = time.Now()
		}
		m, err := c.readMessage()
		if err != nil {
			if err == io.EOF {
				log.Println("[WARNING] Connection closed.")
			} else {
				if c.IsServerConn() {
					if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
						// Handle timeout connection
						if !mux.pool.Prune(c, true) {
							// Keep one connection alive
							// Maybe send a ping?
							continue
						}
						c.Close()
						log.Println("[WARNING] "+ err.Error())
						return
					}
				}
				log.Println("[ERROR] "+ err.Error())
			}
			return
		}
		if m != nil {
			if c.IsClientConn() {
				delta := time.Now().Sub(c.tl_response) / time.Millisecond
				if delta > 3 && delta < 100 {
					go mux.pool.Populate() // Make sure we have maximum bandwidth
				}
			}
			m.process(mux)
		}
	}
}

func (c *wsConn) readMessage() (m message, err error) {
	err = MESSAGE.Receive(c.Conn, &m)
	return
}

func (c *wsConn) sender(mux *Conn) {
	for {
		select {
		case data := <-c.binary:
			if c.IsServerConn() {
				c.SetDeadline(time.Now().Add(ConnTimeout))
			}
			_, err := c.Write(data)
			if err != nil {
				log.Println("[ERROR] "+ err.Error())
				// Queue data to another wsConn?
				return
			}
			mux.pool.Put(c)
		case <-c.ondisconnect:
			return // leaving
		}
	}
}
func (c *wsConn) sendByte(data []byte) {
	c.binary <-data
}

func wrapConn(ws *websocket.Conn, h *Header) *wsConn {
	ws.PayloadType = websocket.BinaryFrame // Default to binary frame for stream
	if h == nil { h = newHeader() } else { h = h.Clone() }
	c := wsConn{ws, make(chan []byte, 256), make(chan bool), h, "", time.Now()}
	return &c
}

type wsDial func() (*wsConn, error)

type wsPool struct {
	dial wsDial
	list chan *wsConn
	count uint8
	max uint8
	lock sync.Mutex
	ondisconnect chan chan bool
}

func newPool(max uint8, dial wsDial) *wsPool {
	return &wsPool{dial:dial, list:make(chan *wsConn, int(max)), max:max,
					ondisconnect:make(chan chan bool, int(max))}
}

func (p *wsPool) Get() (ws *wsConn, err error) {
	Again:
		if p.dial == nil {
			// Pool is not auto populated
			// Wait for an available connection
			ws = <-p.list
			if ws == nil { return nil, ErrConnectionClosed }
			if !ws.isConnected() { goto Again }
			return
		}
		select {
		case ws = <-p.list:
			// Try to get one connection
		default:
			// None available
			p.lock.Lock()
			has_free_slot := p.count < p.max
			if has_free_slot {
				// Max connection is not reached, reserve a place
				p.count++
			}
			p.lock.Unlock()

			if has_free_slot {
				// Open a new connection
				ws, err = p.dial()
				if err != nil {
					// Fail to connect, give up the reserved place 
					p.lock.Lock()
					if p.count > 0 {
						p.count--
					} else {
						// Avoid a racing condition with Close()
						// Looks like Close() was called while a new connection
						// was attempted (it is the only reason why p.count
						// would be decremented in the mean time).
						// Now that the connection attempt failed, no conn would
						// be put into p.list, which means Close() is left
						// waiting for one.
						// We are going to push a nil into the p.list instead,
						// so that Close() may continue iterate.
						p.list <- nil
					}
					p.lock.Unlock()
				} else {
					p.Watch(ws)
				}
				return
			}
			// Wait for an available connection
			ws = <-p.list
		}
		if ws == nil { return nil, ErrConnectionClosed }
		if !ws.isConnected() {
			// Remove lost connection and try again
			p.lock.Lock()
			p.count--
			p.lock.Unlock()
			goto Again
		}
		return
}

func (p *wsPool) Put(ws *wsConn) {
	if ws == nil { return }
	select {
	case p.list <-ws:
		// Put connection into the pool
	default:
		// Max connection reached, flush it
		ws.Close()
	}
}

func (p *wsPool) Close() {
	p.lock.Lock()
	if p.max == 0 {
		// already closed
		p.lock.Unlock()
		return
	}
	i := p.count
	p.count = 0
	p.max = 0
	p.lock.Unlock()
	for ; i > 0; i-- {
		ws := <-p.list
		if ws != nil && ws.isConnected() {
			ws.Close()
		}
	}
	close(p.list)
	close(p.ondisconnect)
}

func (p *wsPool) Prune(target *wsConn, keep_one bool) bool {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.count == 0 { return false }
	if keep_one && p.count == 1 { return false }
	temp := make([]*wsConn, 0, int(p.max))
	defer func() {
		// Put back the conns
		for _, c := range temp {
			p.list <-c
		}
	}()
	for {
		select {
		case ws := <-p.list:
			if ws == target {
				p.count--
				return true
			} else {
				temp = append(temp, ws)
			}
		default:
			// Do not block
			return false
		}
	}
}

func (p *wsPool) Watch(ws *wsConn) {
	p.ondisconnect <-ws.ondisconnect
}

func (p *wsPool) Add(ws *wsConn) {
	p.lock.Lock()
	has_free_slot := p.count < p.max
	if has_free_slot {
		p.count++
	}
	p.lock.Unlock()
	if has_free_slot {
		p.Put(ws)
		p.Watch(ws)
	} else {
		ws.Close()
	}
}

func (p *wsPool) Wait() {
	cases := make([]reflect.SelectCase, 1, int(p.max) +1)
	cases[0] = reflect.SelectCase{Dir: reflect.SelectRecv,
									Chan: reflect.ValueOf(p.ondisconnect)}
	for {
		i, ch, ok := reflect.Select(cases)
		if i == 0 {
			if ok {
				// add another case to watch
				c := reflect.SelectCase{Dir: reflect.SelectRecv, Chan: ch}
				cases = append(cases, c)
			} else { return } // close channel
		} else {
			if ok { panic("Should not happen!") }
			// closed connection, remove it from list
			last := len(cases) -1
			if last == 1 { return } // empty list
			cases[i] = cases[last] // swap with last item
			cases = cases[:last] // truncate the slice
		}
	}
}

func (p *wsPool) Populate() {
	if p.dial == nil { return } // Server side can't dial new connection

	// for { }
	p.lock.Lock()
	has_free_slot := p.count < p.max
	if has_free_slot { p.count++ }
	p.lock.Unlock()
	if !has_free_slot { return }

	ws, err := p.dial()
	if err != nil {
		p.lock.Lock()
		p.count--
		p.lock.Unlock()
		return
	} else {
		p.Watch(ws)
	}
	p.Put(ws)
}

func (p *wsPool) DePopulate() {
	if p.dial == nil { return }

	for {
		p.lock.Lock()
		has_toomuch_slot := p.count > 1
		if has_toomuch_slot { p.count-- }
		p.lock.Unlock()
		if !has_toomuch_slot { return }

		ws := <-p.list
		if ws == nil { return }
		if ws.isConnected() {
			ws.Close()
		}
	}
}

func (p *wsPool) IsMultiplex() bool {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.count > 1
}

// Main connection struct.
type Conn struct {
	pool *wsPool
	srv *service
	request_id *requestId
	pending_request *pendingMap
	streaming *streamingMap
	ondisconnect chan bool // Event channel. Can be used with select statement
	value reflect.Value
	Header *Header
	id string
}
// Use c.IsConnected() or <-c.OnDisconnect() to know when connection is closed.

func newConn(srv *service, dial wsDial) *Conn {
	c := Conn{pool:newPool(srv.max_conn, dial), srv:srv, request_id:&requestId{},
			pending_request:newPendingMap(), streaming:newStreamingMap(),
			ondisconnect:make(chan bool)}
	c.value = reflect.ValueOf(&c)
	return &c
}

func (c *Conn) serve() {
	// handling Connect/Disconnect events
	if c.srv.rcvr != nil {
		go c.srv.rcvr.OnConnect(c)
		defer c.srv.rcvr.OnDisconnect(c)
	}
	c.pool.Wait()
	c.clean()
}

func (c *Conn) init(id string, h *Header) {
	c.Header = h
	c.id = id
	c.srv.putMux(id, c)
}

func (c *Conn) clean() {
	c.pending_request.dropAll(ErrCanceledLostConn)
	c.streaming.dropAll(ErrCanceledLostConn)
	c.srv.removeMux(c.id)
	c.pool.Close()
	close(c.ondisconnect)
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

func (c *Conn) Close() { c.pool.Close() }

func (c *Conn) sendJSON(data interface{}) error {
	ws, err := c.pool.Get()
	if err != nil { return err }
	err = websocket.JSON.Send(ws.Conn, data)
	if err != nil { return err }
	c.pool.Put(ws)
	return nil
}
func (c *Conn) sendByte(data []byte) error {
	ws, err := c.pool.Get()
	if err != nil { return err }
	_, err = ws.Write(data)
	if err != nil { return err }
	c.pool.Put(ws)
	return nil
}

// Send a remote call to the connected service. Return a PendingRequest if successful.
// Sending the request is done synchronously. Waiting for the result is not.
func (c *Conn) RemoteCall(name string, kwargs, reply interface{}) (pending PendingRequest, err error) {
	need_response := (reply != nil)

	var id uint32 = 0
	if need_response {
		id = c.request_id.get()
		pending = newPendingRequest(reply)
		c.pending_request.set(id, pending)
	}
	r := response{ID:id, SV:name, KW:kwargs}

	err = c.sendJSON(&r)
	if err != nil {
		if need_response {
			c.pending_request.pop(id)
			pending = nil
		}
		return
	}

	return
}

func (c *Conn) sendResponse(id uint32, replyv interface{}, err error) {
	r_type := "R"
	if err != nil  {
		replyv = "(remote) "+ err.Error()
		r_type = "ERR"
	}

	r := response{ID: id, SV: r_type, KW: replyv}

	err = c.sendJSON(&r)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
	}
}

// Equivalent to RemoteCall(), but expect a binary stream response.
func (c *Conn) RemoteStream(name string, kwargs interface{}) (pending StreamReceiver, err error) {
	id := c.request_id.get()

	r := response{ID:id, SV:name, KW:kwargs}

	pending = newPendingWriter(id, c)
	c.pending_request.set(id, pending)

	err = c.sendJSON(&r)
	if err != nil {
		c.pending_request.pop(id)
		pending = nil
		return
	}

	return
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
	seq uint16
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

	if s.sendPacket(endOfStream(s.id_b, seq)) == nil { s.done() }
}

func(s *streamer) sendPacket(packet stream_packet) error {
	// return s.c.sendByte(packet) // One Connection
	// Multiple Connections
	ws, err := s.c.pool.Get()
	if err != nil { return err }
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
	if err != nil { return }
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
		if err != nil { return }

		length -= uint64(nr)
		s.progress.Lock()
		s.packet_sent++
		s.progress.Unlock()
	}

	// Send end of feed
	seq++
	err = s.sendPacket(endOfStream(id_b, seq))
	if err != nil { return }

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

func (l *byteList) Pop() []byte {
	first_p := l.head
	if first_p == nil { return nil }
	l.head = first_p.next
	if l.head == nil { l.tail = nil }
	return first_p.data
}

func (l *byteList) IsEmpty() bool {
	return l.head == nil
}

type pendingWriter struct {
	pendingRequest
	id uint32
	c *Conn
	queue *byteList
	towrite chan bool
	seq_cache [65536][]byte
	seq uint16
	writing sync.Mutex
	progress sync.RWMutex
	packet_total uint64
	packet_sent uint64
}

func newPendingWriter(id uint32, c *Conn) StreamReceiver {
	return &pendingWriter{
		pendingRequest:pendingRequest{ondone:make(chan bool)},
		id:id, c:c, towrite:make(chan bool, 1), queue:new(byteList),
	}
}

func (p *pendingWriter) write(s stream_packet) (done bool) {
	p.writing.Lock()
	defer p.writing.Unlock()

	// // One Connection
	// data := s.get_data()
	// p.notifyToWrite()
	// if len(data) == 0 {
	// 	// Empty data means end of feed
	// 	return true
	// }
	// p.queue.Append(data)
	// return false

	// Multiple Connection
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
		p.seq, done = p.getBuffer(seq)

	} else {
		p.seq_cache[seq] = data
	}
	return
}

func (p *pendingWriter) notifyToWrite() {
	select { // Notify data is ready
	case p.towrite <-true:
	default :
	}
}

func (p *pendingWriter) getBuffer(seq uint16) (uint16, bool) {
	loop:
		next_seq := seq + 1
		data := p.seq_cache[next_seq]
		if data != nil {
			if len(data) > 0 {
				p.queue.Append(data)
				p.seq_cache[next_seq] = nil
				seq = next_seq
				goto loop
			}
			// Empty data means end of feed
			return seq, true
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
		if !p.queue.IsEmpty() && !p.IsDone() { p.notifyToWrite() }
		p.writing.Unlock()
	}
	return data, p.error
}

func (p *pendingWriter) ReceiveFile(dst io.Writer) error {
	// Very first packet is payload
	data, err := p.Receive()
	if data == nil { return err }
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


