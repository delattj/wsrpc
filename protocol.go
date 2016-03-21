package wsrpc

import (
	"log"
	"io"
	// "fmt"
	"time"
	"net/http"
	"golang.org/x/net/websocket"
	"encoding/json"
	"reflect"
	"sync"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Message struct { // When receiving
	ID uint32          // ID:0 means no return value
	SV string          // SV:"R" => Response, SV:"ERR" => Error report, the rest is a request
	KW json.RawMessage // to decode later depending of SV
}

type Response struct { // When sending
	ID uint32
	SV string
	KW interface{}
}

// Precompute the reflect type for error.  Can't use error directly
// because Typeof takes an empty interface value.  This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()

var (
	ErrTimeout					= errors.New("wsrpc: Task timeout")
	ErrCanceled					= errors.New("wsrpc: Canceled task. Lost remote connection.")
	ErrUnsolicitedResponse		= errors.New("wsrpc: unsolicited response")
	ErrUnknownPendingRequest	= errors.New("wsrpc: unknown pending request id")
	ErrNoServices				= errors.New("wsrpc: no registered services")
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
		msg, err := c.readMessage()
		if err != nil {
			if err == io.EOF {
				log.Println("[WARNING] Connection closed.")

			} else {
				log.Println("[ERROR] "+ err.Error())
			}
			break
		}

		switch msg.SV {
		case "ERR":
			fallthrough
		case "R":
			err := c.handleResponse(msg)
			if err != nil {
				log.Println("[ERROR] "+ err.Error())
			}

		default:
			mtype, argv, replyv, err := c.handleRequest(msg)
			if err != nil {
				if msg.ID != 0 {
					go c.sendResponse(msg.ID, reflect.Value{}, err)

				} else {
					log.Println("[ERROR] "+ err.Error())
				}

			} else {
				go c.handleCall(msg.ID, mtype, argv, replyv)
			}
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
		if replyType.Kind() != reflect.Ptr {
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
type PendingRequest struct {
	Reply interface{}
	OnDone chan bool // Event channel. Can be used with select statement
	Error error
}
// Use p.Wait(), p.WaitTimeout(t) or <-p.OnDone to know when the reply value is ready.
// p.Wait() and p.WaitTimeout(t) return p.Error or wsrpc.ErrTimeout.
// Reply value and Error message are stored respectively inside p.Reply and p.Error
// You should read those fields only when p.OnDone is released.

func newPendingRequest(reply interface{}) *PendingRequest {
	return &PendingRequest{Reply:reply, OnDone:make(chan bool)}
}

func (p *PendingRequest) done() {
	close(p.OnDone)
}

func (p *PendingRequest) Wait() error {
	<-p.OnDone
	return p.Error
}

// Like Wait() but return wsrpc.ErrTimeout if timeout is reached
func (p *PendingRequest) WaitTimeout(t time.Duration) error {
	ticker := time.NewTicker(t)
	defer ticker.Stop()
	
	select {
	case <-p.OnDone:
		return p.Error

	case <-ticker.C:
		return ErrTimeout
	}
}

func (p *PendingRequest) HasFailed() bool { return p.Error != nil }

// Main connection struct.
type Conn struct {
	ws *websocket.Conn
	srv *service
	sending sync.Mutex
	request_id uint32
	pending sync.Mutex
	pending_request map[uint32]*PendingRequest
	OnDisconnect chan bool // Event channel. Can be used with select statement
	value reflect.Value
}
// Use c.IsConnected() or <-c.OnDisconnect to know when connection is closed.

var typeOfConn = reflect.TypeOf((*Conn)(nil)).Elem()

func newConn(ws *websocket.Conn, srv *service) *Conn {
	c := Conn{ws:ws, srv:srv}
	c.value = reflect.ValueOf(&c)
	return &c
}

func (c *Conn) connected() {
	c.OnDisconnect = make(chan bool)
}

func (c *Conn) IsConnected() bool {
	select {
	case <-c.OnDisconnect:
		return false
	default:
		return true
	}
}

func (c *Conn) disconnected() {
	close(c.OnDisconnect)
	c.cancelPending()
}

func (c *Conn) Close() error { return c.ws.Close() }
func (c *Conn) Request() *http.Request { return c.ws.Request() }
func (c *Conn) SetDeadline(t time.Time) error { return c.ws.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error { return c.ws.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// Send a remote call to the connected service. Return a PendingRequest if successful.
// Sending the request is done synchronously. Waiting for the result is not.
func (c *Conn) RemoteCall(name string, kwargs, reply interface{}) (pending *PendingRequest, err error) {
	need_response := (reply != nil)

	c.sending.Lock()
	defer c.sending.Unlock()

	var id uint32 = 0
	if need_response {
		c.request_id++
		if c.request_id == 0 {
			c.request_id++
		}
		id = c.request_id
	}
	r := Response{ID:id, SV:name, KW:kwargs}

	err = websocket.JSON.Send(c.ws, &r)

	if err != nil {
		return
	}

	if !need_response {
		return
	}

	c.pending.Lock()

	if c.pending_request == nil {
		c.pending_request = make(map[uint32]*PendingRequest)
	}

	pending = newPendingRequest(reply)
	c.pending_request[id] = pending

	c.pending.Unlock()

	return
}

func (c *Conn) readMessage() (msg *Message, err error) {
	msg = new(Message)
	err = websocket.JSON.Receive(c.ws, msg)
	return
}

func (c *Conn) handleResponse(resp *Message) (err error) {
	c.pending.Lock()
	defer c.pending.Unlock()

	if c.pending_request == nil {
		return ErrUnsolicitedResponse
	}
	pending := c.pending_request[resp.ID]
	if pending == nil {
		return ErrUnknownPendingRequest
	}
	delete(c.pending_request, resp.ID)
	defer pending.done()

	// Decode Response content(KW) depending of the SV type (error or reply data)
	if resp.SV == "ERR" {
		var errMsg string
		err = json.Unmarshal(resp.KW, &errMsg)
		pending.Error = errors.New(errMsg)
		return
	}
	
	err = json.Unmarshal(resp.KW, pending.Reply)

	return
}

func (c *Conn) handleRequest(req *Message) (mtype *methodType, argv, replyv reflect.Value, err error) {

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
	mtype = s.method[methodName]
	if mtype == nil {
		err = errors.New("wsrpc: can't find method "+ methodName)
		return
	}

	// Create the argument value.
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

	replyv = reflect.New(mtype.ReplyType.Elem())
	return
}

func (c *Conn) handleCall(id uint32, mtype *methodType, argv, replyv reflect.Value) {
	err := c.call(mtype, argv, replyv)

	if id != 0 {
		c.sendResponse(id, replyv, err)

	} else if err != nil {
		log.Println("[ERROR] "+ err.Error())
	}
}

func (c *Conn) call(mtype *methodType, argv, replyv reflect.Value) (err error) {
	// defer func() { // Trap panic ???
	// 	if r := recover(); r != nil {
	// 		err = errors.New(fmt.Sprintf("%s", r))
	// 	}
	// }()
	// Invoke the method, providing a new value for the reply.
	function := mtype.method.Func
	returnValues := function.Call([]reflect.Value{c.srv.v_rcvr, c.value, argv, replyv})
	// The return value for the method is an error.
	errInter := returnValues[0].Interface()
	if errInter != nil {
		err = errInter.(error)
	}
	return
}

func (c *Conn) sendResponse(id uint32, replyv reflect.Value, err error) {
	r_type := "R"
	if err != nil  {
		replyv = reflect.ValueOf("(remote) "+ err.Error())
		r_type = "ERR"
	}

	r := Response{ID: id, SV: r_type, KW: replyv.Interface()}

	c.sending.Lock()
	err = websocket.JSON.Send(c.ws, &r)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
	}
	c.sending.Unlock()
}

func (c *Conn) cancelPending() {
	c.pending.Lock()
	if c.pending_request != nil {
		for _, pending := range c.pending_request {
			pending.Error = ErrCanceled
			pending.done()
		}
		c.pending_request = nil
	}
	c.pending.Unlock()
}
