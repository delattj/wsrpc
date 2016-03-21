package main

import (
	"os"
	"net"
	"log"
	"syscall"
	"os/signal"
	"../../../wsrpc"
	"../data"
)

type Node struct {
	Mac string
}

func (n *Node) Dispatch(cnx *wsrpc.Conn, kwargs *data.Work, reply wsrpc.Nothing) (err error) {
	log.Printf("[INFO] %s\n", kwargs.Name)
	return
}

func (n *Node) OnConnect(cnx *wsrpc.Conn) {
	log.Println("[INFO]", n.Mac)

	var i int
	task, err := cnx.RemoteCall("Node.RegisterMac", n.Mac, &i)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
		return
	}
	err = task.Wait()
	if err != nil {
		log.Printf("[ERROR] Registering: %s\n", err)
		return
	}
	log.Println("[INFO] Node registered")
}

func (n *Node) OnDisconnect(cnx *wsrpc.Conn) {
}

func OnInterrupt(callback func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		<-c
		callback()
		os.Exit(1)
	}()
	log.Println("[INFO] Ctrl+Break to stop")
}

func main() {

	// Command line arguments
	server := "localhost:8080"
	if len(os.Args) > 1 {
		server = os.Args[1]
	}

	// Get Mac Address
	ifs, _ := net.Interfaces()
	v := ifs[0]
	mac := v.HardwareAddr.String()

	// Initialize Service
	n := new(Node)
	n.Mac = mac

	ws := wsrpc.NewNode("ws://"+ server +"/node", n)
	ws.SetReconnect(5) // If disconnected, try reconnecting every 5 seconds

	// Trap Keyboard interrupt and start service
	OnInterrupt(func() {ws.Close()})
	ws.Serve()

}