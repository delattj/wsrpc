package wsrpc

import (
	"log"
	"time"
	"sync"
	"errors"
	"golang.org/x/net/websocket"
)

type Node struct {
	conn *Conn
	Url string
	Origin string
	srv *service
	connected sync.WaitGroup
	reconnect uint16
}

func (n *Node) connect() (err error) {
	var ws *websocket.Conn
	ws, err = websocket.Dial(n.Url, "", n.Origin)
	if err != nil {
		return
	}
	if n.conn == nil {
		n.conn = newConn(ws, n.srv)

	} else {
		// Reconnection, we swap only the websocket.Conn
		n.conn.ws = ws
	}
	n.connected.Done()

	log.Printf("[INFO] Connected to %s\n", n.Url)
	return
}

func (n *Node) WaitConnected() {
	n.connected.Wait()
}

// Enable reconnection every <elapse> seconds until successful
func (n *Node) SetReconnect(elapse uint16) {
	n.reconnect = elapse
}

func (n *Node) Serve() {
	for {
		err := n.connect()
		if err != nil {
			log.Printf("[ERROR] %s\n", err)
			if n.reconnect == 0 {
				break
			}
			time.Sleep(time.Duration(n.reconnect) *time.Second)

		} else {
			n.conn.serve()
			// Lost connection
			if n.reconnect == 0 {
				break
			}
			n.connected.Add(1)
		}
	}
	return
}

func (n *Node) Close() error {
	if c := n.conn; c != nil {
		return c.Close()
	}
	return errors.New("Node is not connected.")
}

func (n *Node) RemoteCall(name string, kwargs, reply interface{}) (pending *PendingRequest, err error) {
	n.WaitConnected()
	return n.conn.RemoteCall(name, kwargs, reply)
}

func NewNode(url string, s Service) *Node {
	srv := service{}
	if s != nil {
		srv.register(s)
	}

	n := Node{Url: url, Origin: url, srv: &srv}
	n.connected.Add(1)
	return &n
}
