package main

import (
	"os"
	"log"
	"sync"
	"net/http"
	"html/template"
	"../../../wsrpc"
	"../data"
)

var (
	Nodes *NodesManager
)


type Page struct {
	Server string
}

func renderTemplate(w http.ResponseWriter, tmpl string, p *Page) {
	t, _ := template.ParseFiles("templates/"+ tmpl)
	t.Execute(w, p)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	p := &Page{Server: r.Host}
	renderTemplate(w, "base.html", p)
}

type Web struct {}

func (w *Web) Dispatch(cnx *wsrpc.Conn, kwargs, reply *data.Work) (err error) {
	log.Printf("[INFO] From Web, dispatch: %s\n", kwargs.Name)

	go Nodes.DispatchAll(kwargs)

	*reply = *kwargs
	return
}

func (w *Web) OnConnect(cnx *wsrpc.Conn) {
	log.Println("[INFO] Web connected")
}

func (w *Web) OnDisconnect(cnx *wsrpc.Conn) {
}

type NodesManager struct {
	nodes map[string]*wsrpc.Conn
	sync sync.Mutex
}

func NewNodesManager() *NodesManager {
	n := NodesManager{}
	n.nodes = make(map[string]*wsrpc.Conn)

	return &n
}

func (m *NodesManager) RegisterCnx(mac string, cnx *wsrpc.Conn) {
	m.sync.Lock()
	defer m.sync.Unlock()

	m.nodes[mac] = cnx
}

func (m *NodesManager) UnregisterCnx(cnx *wsrpc.Conn) (mac string) {
	m.sync.Lock()
	defer m.sync.Unlock()

	for k, v := range m.nodes {
		if v == cnx {
			mac = k
			break
		}
	}

	if mac != "" {
		delete(m.nodes, mac)
	}

	return
}

func (m *NodesManager) DispatchAll(w *data.Work) {
	m.sync.Lock()
	defer m.sync.Unlock()

	for mac, cnx := range m.nodes {
		_, err := cnx.RemoteCall("Node.Dispatch", w, nil)
		if err != nil {
			log.Printf("[ERROR] %s\n", err)
		} else {
			log.Printf("[INFO] Work '%s' dispatched to %s\n", w.Name, mac)
		}
	}
}

type Node struct {}

func (n *Node) RegisterMac(cnx *wsrpc.Conn, mac string, reply *int) (err error) {
	log.Printf("[INFO] Registering %s\n", mac)

	Nodes.RegisterCnx(mac, cnx)

	*reply = 1
	return
}

func (n *Node) GetFile(cnx *wsrpc.Conn, filename string, stream wsrpc.StreamSender) (err error) {
	log.Printf("[INFO] Requested file %s\n", filename)

	file, err := os.Open(filename)
	if err != nil { return }
	info, err := file.Stat()
	if err != nil { return }

	err = stream.SendFile(file, uint64(info.Size()))
	if err != nil {
		log.Printf("[ERROR] %s\n", err)
		return
	}

	file.Close()

	log.Printf("[INFO] Done\n")
	
	return
}


func (n *Node) OnConnect(cnx *wsrpc.Conn) {
	log.Println("[INFO] Node connected")
}

func (n *Node) OnDisconnect(cnx *wsrpc.Conn) {
	log.Println("[INFO] Node disconnected")

	mac := Nodes.UnregisterCnx(cnx)
	if mac != "" {
		log.Println("[INFO] Unregistered:", mac)
	}
}


func main() {

	// Command line arguments
	port := "8080"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	Nodes = NewNodesManager()

	// Register wsrpc services on the http server
	http.Handle("/node", wsrpc.Handler(new(Node))) // ws://<host>:<port>/node
	http.Handle("/web", wsrpc.Handler(new(Web))) // ws://<host>:<port>/web

	// static files
	http.Handle("/static/",
		http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// index
	http.HandleFunc("/", indexHandler)

	log.Println("[INFO] Serving through:", "localhost:"+ port)

	err := http.ListenAndServe(":"+ port, nil)
	if err != nil {
		log.Println("[ERROR] (Server) " + err.Error())
	}
}
