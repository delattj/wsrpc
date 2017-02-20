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
	ErrUnexpectedJSONPacket		= errors.New("wsrpc: Did not expect a json packet for this pending request.")
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
				if delta > 6 && delta < 100 {
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
