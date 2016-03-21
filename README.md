# wsrpc - Go bi-directional RPC over Websocket
## Introduction

This library came to be as a need for an unify RPC protocol that could received remote request from different language and unstandard client-slave or both way request.
**Websocket** is used as transport layer and **JSON** as message encoding.
This make it easy to be implemented in Javascript and other language like Go or Python.

This paradigm makes it possible to have agents connect to a central server as RPC *server* and client *front-end* to the central server as RPC *client*. The central server is basically a **hub**.

The library was first created based on Go net/rpc code. wsrpc does share the same API implementation in many ways.

## Sample

See **sample_dispatch** project for an implementation example.
The example include a Javascript version of the protocol. However it supports only sending requests and receiving responses.

## Getting started

### Vocabulary

**Nodes** are RPC agents that connect to a websocket server. They may or may not exposed services. They can send and receive RPC request.
**Servers** are websocket server that accept connection from *Nodes*. They may or may not exposed services. They can send RPC and receive request.

### Nodes
Nodes are created using:
```go
node := wsrpc.NewNode(url string, s Service)
```
**url** must be a value Websocket address (ie: ws://myserver/node).
**s** can be a valid Service(see below) or nil if you don't need to exposed any service.
You then typically run the following method to connect and serve request:
```go
node.Serve()
```
You probably want to run this in a goroutine as it is blocking.
You can then send remote request with:
```go
node.RemoteCall(name string, kwargs, reply interface{}) (pending *PendingRequest, err error)
```
The method will first wait for the node to connect to the serve; then it blocks for the time to send the data over. But the result will be fulfilled by **node.Serve()**. In the meantime use the return PendingRequest object to know when the result will be available.
To do so, you can do:
```go
pending.Wait() error
```
The method is blocking.
The return error is the remote error if any, otherwise return nil.
The reply value will be avaible at this point.
You can use PendingRequest.OnDone channel as an alternate way to synchronize.

### Server
Use *wsrpc.Handler* to register the services on the Go standard http server:
```go
http.Handle(path, wsrpc.Handler(s Service))
```
You then run the server as usual:
```go
http.ListenAndServe(":"+ port, nil)
```
Remote call are possible through the *wsrpc.Conn* object passed to the services and events.

### Service
**Service** is an interface that require this signatures:
```go
type Service interface {
	OnConnect(c *wsrpc.Conn)
	OnDisconnect(c *wsrpc.Conn)
}
```
*OnConnect* and *OnDisconnect* are events callback triggered at the propriate time.
*wsrpc.Conn* represent the connection to the remote end. Through that instance you may send remote request or query connection (ie: conn.IsConnected()).
To expose service methods they must satify these criteria:
 - the method's type is exported.
 - the method is exported.
 - the method has three arguments
 - the first is a pointer to *wsrpc.Conn*
 - the second is the passed argument, it need to be exported (or builtin) types and JSON encodable.
 - the last one is the returned object, a pointer to an instance that need to be exported (or builtin) types and JSON encodable.
 - the method has return type error.
Signature looks like:
```go
func (t *T) MethodName(conn *wsrpc.Conn, argType T1, replyType *T2) error
```
Any other method will be ignored.

Here a simple example:
```go
type MyServices struct {
}

func (t *MyServices) MyFunc(cnx *wsrpc.Conn, kwargs *data.Kwargs, reply *string) (err error) {
	log.Printf("[INFO] %s\n", kwargs.A)
	*reply = kwargs.A
	return
}

func (t *MyServices) OnConnect(cnx *wsrpc.Conn) {
	log.Println("[INFO] OnConnect")
}

func (t *MyServices) OnDisconnect(cnx *wsrpc.Conn) {
	log.Println("[INFO] OnDisconnect")
}

```

### Remote Call

You can send remote request through **wsrpc.Node** or **wsrpc.Conn**(while inside a service method):
```go
conn.RemoteCall(name string, kwargs, reply interface{}) (pending *PendingRequest, err error)
```
The method will first wait for the node to connect to the serve; then it blocks for the time to send the data over. But the result will be fulfilled by **node.Serve()** or **wsrpc.Handler**. In the meantime use the return *PendingRequest* object to know when the result will be available.
To do so, you can do:
```go
pending.Wait() error
```
The method is blocking.
The return error is the remote error if any, otherwise return nil.
The reply value will be avaible at this point.
You can use PendingRequest.OnDone channel as an alternate way to synchronize.

## API Reference

### type Node
**TODO**

### type Conn
**TODO**

### type PendingRequest
**TODO**
