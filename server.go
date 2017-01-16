package wsrpc

import (
	"log"
	"golang.org/x/net/websocket"
)


func Handler(s Service, max uint8) websocket.Handler {
	srv := newService()
	srv.maxSocket(max)
	srv.register(s)
	return websocket.Handler(
		func (ws *websocket.Conn) {
			c := wrapConn(ws, nil)
			if err := c.validate(srv); err != nil {
				log.Println("[ERROR] "+ err.Error())
				return
			}
			mux := srv.getMux(c.id)
			need_init := mux == nil
			if need_init {
				mux = newConn(srv, nil)
				mux.init(c.id, c.header)
			}
			mux.pool.Add(c)
			if need_init { go mux.serve() }
			c.serve(mux)
		},
	)
}
