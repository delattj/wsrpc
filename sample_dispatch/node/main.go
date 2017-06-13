package main

import (
	"../data"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
	"wsrpc"
)

type Node struct {
	Mac      string
	Filename string
}

func (n *Node) Dispatch(cnx *wsrpc.Conn, kwargs *data.Work, reply wsrpc.Nothing) (err error) {
	log.Printf("[INFO] %s\n", kwargs.Name)
	return
}

func (n *Node) OnHandshake(header *wsrpc.Header) error {
	log.Printf("[###]")
	return nil
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

	if n.Filename != "" {
		go GetFile(cnx, n.Filename)
		// go WrongTypeCall(cnx, n.Filename)
	}

}

func (n *Node) OnDisconnect(cnx *wsrpc.Conn) {
}

func WrongTypeCall(cnx *wsrpc.Conn, filename string) {

	var i int
	task, err := cnx.RemoteCall("Node.GetFile", filename, &i)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
		return
	}
	err = task.Wait()
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
		return
	}
	log.Println("[INFO] No Error???")
}

func GetFile(cnx *wsrpc.Conn, filename string) {

	log.Println("[INFO] Requesting file:", filename)
	stream, err := cnx.RemoteStream("Node.GetFile", filename)
	// stream, err := cnx.RemoteStream("Node.WrongTypeReponse", filename)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
		return
	}

	file, err := os.Create(filename)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
		stream.Cancel()
		return
	}

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var l, p uint64
		for l == 0 || l != p {
			select {
			case <-stream.OnDone():
				return

			case <-ticker.C:
				p, l = stream.Progress()
				if l > 0 {
					pp := p * 100 / l
					log.Printf("[PROGRESS] %v%%\n", pp)
				}
			}
		}
	}()

	err = stream.ReceiveFile(file)
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
		return
	}

	file.Close()

	log.Printf("[INFO] File transfer done.\n")
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
	var filename string
	var server string
	var max_socket uint
	flag.StringVar(&filename, "filename", "", "File to download.")
	flag.StringVar(&server, "server", "localhost:8080", "Server to connect to.")
	flag.UintVar(&max_socket, "max_socket", 2, "Maximum number of parallel socket connection.")
	flag.Parse()

	// Get Mac Address
	ifs, _ := net.Interfaces()
	v := ifs[0]
	mac := v.HardwareAddr.String()

	// Initialize Service
	n := new(Node)
	n.Mac = mac
	n.Filename = filename

	ws := wsrpc.NewNode("ws://"+server+"/node", n)
	ws.SetMaxSocket(uint8(max_socket))
	ws.SetReconnect(5) // If disconnected, try reconnecting every 5 seconds

	// Trap Keyboard interrupt and start service
	OnInterrupt(func() { ws.Close() })
	ws.Serve()

}
