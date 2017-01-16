package wsrpc

import (
	"log"
	"time"
	"sync"
	"errors"
	"golang.org/x/net/websocket"
)

type connSafe struct {
	ch chan *Conn
}

func (cs *connSafe) get() (c *Conn) {
	c = <-cs.ch
	cs.ch <- c
	return
}
func (cs *connSafe) set(c *Conn) {
	<-cs.ch
	cs.ch <- c
}
func newConnSafe() *connSafe {
	ch := make(chan *Conn, 1)
	ch <- nil
	return &connSafe{ch}
}

type Node struct {
	conn *connSafe
	Url string
	Origin string
	srv *service
	connected sync.WaitGroup
	reconnect uint16
}

func (n *Node) dial() (c *wsConn, err error) {
	var ws *websocket.Conn
	ws, err = websocket.Dial(n.Url, "", n.Origin)
	if err != nil { return }

	mux := n.conn.get()
	c = wrapConn(ws, mux.Header)
	err = c.validate(n.srv)
	if err != nil { panic(err) }
	go c.serve(mux)
	return
}

func (n *Node) connect() (err error) {
	var ws *wsConn
	mux := newConn(n.srv, n.dial)
	n.conn.set(mux)
	ws, err = mux.pool.Get()
	if err != nil { return }
	mux.init(ws.id, ws.header)
	mux.pool.Put(ws)

	n.connected.Done()

	log.Printf("[INFO] Connected to %s\n", n.Url)
	return
}

func (n *Node) WaitConnected() {
	n.connected.Wait()
}

func (n *Node) GetConnection() *Conn {
	n.connected.Wait()
	return n.conn.get()
}

// Enable reconnection every <elapse> seconds until successful
func (n *Node) SetReconnect(elapse uint16) {
	n.reconnect = elapse
}

func (n *Node) SetMaxSocket(max uint8) {
	n.srv.maxSocket(max)
}

func (n *Node) Serve() {
	for {
		err := n.connect()
		if err != nil {
			n.conn.set(nil)
			log.Printf("[ERROR] %s\n", err)
			if n.reconnect == 0 { break }
			time.Sleep(time.Duration(n.reconnect) *time.Second)

		} else {
			n.conn.get().serve()
			// Lost connection
			n.conn.set(nil)
			if n.reconnect == 0 { break }
			n.connected.Add(1)
		}
	}
}

func (n *Node) Close() error {
	if c := n.conn.get(); c != nil {
		c.Close()
		return nil
	}
	return errors.New("Node is not connected.")
}

func NewNode(url string, s Service) *Node {
	srv := newService()
	if s != nil {
		srv.register(s)
	}

	n := Node{Url: url, Origin: url, srv: srv, conn: newConnSafe()}
	n.connected.Add(1)
	return &n
}
