package wsrpc

import (
	"golang.org/x/net/websocket"
)


func Handler(s Service) websocket.Handler {
	srv := service{}
	srv.register(s)
	return websocket.Handler(
		func (ws *websocket.Conn) {
			c := newConn(ws, &srv)
			c.serve()
		},
	)
}
