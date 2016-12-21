# wsrpc - Go bi-directional RPC over Websocket
## Introduction

This library came to be as a need for an unify RPC protocol that could received remote request from different language and unstandard client-slave or both way request.  
**Websocket** is used as transport layer and **JSON** as message encoding.  
This make it easy to be implemented in Javascript and other language like Go or Python.
The protocol also support **binary stream** through an opened channel and an interface to send file like object.  
There is the possibility to use a pool of parallel connections to receive and send data. Binary data is resequenced on arrival.

This paradigm makes it possible to have agents connect to a central server as RPC *server* and client *front-end* to the central server as RPC *client*. The central server is basically a **hub**.

The library was first created based on Go net/rpc code. wsrpc does share the same API implementation in many ways.

## Sample

See **sample_dispatch** project for an implementation example.  
The example include a Javascript version of the protocol. However it supports only sending requests and receiving responses.  
Compile **server** and **node**. Start the *server* executable, then you can connect *nodes* (note that only one node per MAC address is supported). To dispatch message connect to the server via a web browser (ie: http://localhost:8080/).

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
You can then request the handle to the connection through the method:
```go
conn := node.GetConnection()
```
The method will first wait for the node to connect to the server; then it return the connection object **Conn** from there you will be able to Use the Conn interface to send request. See Remote Call section below

### Server
Use **wsrpc.Handler** to register the services on the Go standard http server:
```go
http.Handle(path, wsrpc.Handler(s Service))
```
You then run the server as usual:
```go
http.ListenAndServe(":"+ port, nil)
```
Remote call are possible through the **wsrpc.Conn** object passed to the services and events.

### Service
**Service** is an interface that require this signatures:
```go
type Service interface {
	OnHandshake(*wsrpc.Header) error
	OnConnect(*wsrpc.Conn)
	OnDisconnect(*wsrpc.Conn)
}
```
**OnHandshake** give the opportunity to implement custom handshake by setting properties to **wsrpc.Header**. The client side is sending the header first and wait for a returned header. The server validate the properties in the header and send back a header. If the properties are not valid, return an error. The client then receive the header back. If the header contains the '**error**' property the connection is aborded.  
When **OnHanshake()** is called on either side this is the opportunity to add (client side) or validate (server side) values.  
**OnConnect** and **OnDisconnect** are events callback triggered at the apropriate time.  
**OnConnect()** is called just after **OnHandshake()**. **OnDisconnect()** is called just after the socket connection is closed.  
**wsrpc.Conn** represent the connection to the remote end. Through that instance you may send remote request or process some clean up.  

**To expose service** methods they must satify these criteria:
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

func (t *MyServices) OnHandshake(h *wsrpc.Header) error {
	return nil
}

func (t *MyServices) OnConnect(cnx *wsrpc.Conn) {
	log.Println("[INFO] OnConnect")
}

func (t *MyServices) OnDisconnect(cnx *wsrpc.Conn) {
	log.Println("[INFO] OnDisconnect")
}

```

For binary stream, the reply type must be **wsrpc.StreamSender**
```go
func (t *MyServices) Channel(cnx *wsrpc.Conn, name string, stream wsrpc.StreamSender) (err error) {
	c := t.Channel.Subscribe(name)
	for {
		select {
		case data := <- c:
			s_err := stream.Send(data)
			if s_err != nil { return } // Connection lost

		case <-stream.OnDone():
			return // Stream closed for some reason
		}
	}
}
```
**wsrpc.StreamSender** provide several convenient methods to deal with sending binary data.  
Support for *[]bytes* and *io.Reader* are provided, see API references for more details.

### Remote Call

You can send remote request through **wsrpc.Conn**:
```go
conn.RemoteCall(name string, kwargs, reply interface{}) (pending PendingRequest, err error)
```
The method will first wait for the node to connect to the serve; then it blocks for the time to send the data over. But the result will be fulfilled by **node.Serve()** or **wsrpc.Handler**. In the meantime use the return *PendingRequest* object to know when the result will be available.

To do so, you can do:
```go
pending.Wait() error
```
The method is blocking.  
The return error is the remote error if any, otherwise return nil.  
The reply value will be available at this point.  
You can use **PendingRequest.OnDone()** to retrieve a channel that will be closed when the remote call is finished as an alternate way to synchronize.

If no response is required, it is possible to prevent a **PendingRequest** to be generated by simply passing **nil** as *reply*.  
The same way if no argument is needed to be sent over to the remote server you may pass **nil** as *kwargs*.  
Conveniently type **wsrpc.Nothing** may be used as type for *kwargs* and *reply* in a service signature to design an argument that is not required.

### Binary Stream Answer

If you require a binary streamed answer, usefull for pub/sub or large data transfer, your remote call need to be done through that method:
```go
conn.RemoteStream(name string, kwargs interface{}) (stream StreamReceiver, err error)
```
It works like **RemoteCall()**, but the reply data will always be in binary form. Notice that the method takes no reply argument. Instead **StreamReceiver** will provide methods to deal with incoming data stream.  
```go
bytesData, err := stream.Receive()
```
The method wait until the incoming binary data packet is received.  
If bytes data is empty and err is nil, the stream has been closed.  
You can also receive data directly in a file like object through a **io.Writer** interface with this method:
```go
err := stream.ReceiveFile(fileWriter)
```
Binary packets are writen directly into the *io.Writer* object. The method block until the stream is closed.  
When data are received this way, the first packet sent is expected to be the payload encoded as a uint64 in big-endian for tracking purposes. Consider this when sending data from the other end. There is a simplify api that does send that packet automaticaly, see **StreamSender.SendFile(src io.Reader)**.  
Note that **StreamReceiver** implement the **PendingRequest** interface as well.

Be aware that not reading from the stream receiver might fill up the buffer and block your websocket channel.

## Message encoding aka JSON Layer

To encode message, all types are single objects, serialized using JSON.  
Request and response share the same JSON layout:  
 - **ID**, an ID used by the sender to relate to the request.  
 It must be a **uint32**. **0** means no return value, even error will be ignored.  
 - **SV**, a **string** used to differenciate request from response or error report.  
 A response will have "**R**" as **SV** value, an error "**ERR**", a cancel stream request "**CANCEL**", while a request will be any other value.  
 We strongly suggest to use dotted syntax to structure your service names, but this is not enforced.  
 - **KW**, it can be any JSON types. For error report, a simple **string** is expected.  

By default a response is expected after a request. But you have the option to discard that worflow by setting a request **ID** to **0**.  
If no value need to be sent or returned **KW** must be set to **null**. When sending an error report, **KW** is expected to be a simple string.  

### Examples:
```javascript
{"ID": 1001, "SV": "Hello", "KW": null} //Request
{"ID": 1001, "SV": "R", "KW": null} //Response
{"ID": 1002, "SV": "Math.Sum", "KW": {"values": [1, 2]}} //Request
{"ID": 1002, "SV": "R", "KW": 3} //Response
{"ID": 1003, "SV": "Math.Divide", "KW": {"A": 1, "B": 0}} //Request
{"ID": 1003, "SV": "ERR", "KW": "Division by 0"} //Response
{"ID": 0, "SV": "Send.NoReply", "KW": null} //Request
```

## Binary data packet

When sending binary data, these packet are identify in two ways. The first 4 bytes are the request ID, the next 2 bytes are the sequence number (used to order packet in case of parallel connections). The rest is the actual binary data.  
 0                   1                   2                   3  
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1  
+---------------------------------------------------------------+  
|                      ID Token (4 bytes)                       |  
+-------------------------------+-------------------------------+  
|     Sequence (2 bytes)        |      Payload Data             |  
+ - - - - - - - - - - - - - - - + - - - - - - - - - - - - - - - +  
|                                                               |  
.                                                               .  
The very first packet sequence number must be 1, it is a uint16 encoded in big endian.  
When sending data through **SendFile()**, the very first packet contains a data that represent the payload size encoded in uint64 big endian. The follwing data packets are sent by chuncks of size equal to **wsrpc.MaxChunkSize** or less (for the last packet).  
This is only true when sending data through **SendFile()** otherwise the size is arbitrary.  
When closing a stream an *End of Stream* packet is sent. This packet contains an empty data (payload of 0).  

## API Reference

### Constructors:
```go
wsrpc.Handler(s Service) websocket.Handler
```
Use to create handler compatible with **http.Handle()**. To create valid **Service** refer to above *Service* section.

```go
wsrpc.NewNode(url string, s Service) wsrpc.Node
```
**url** must be a valid address to a websocket server (ie: ws/localhost:8080/node).

### type Node
```go
type Node struct {
	Url string
	Origin string
}
```

```go
func (n *Node) WaitConnected()
```
Block until Node is connected to a server.

```go
func (n *Node) SetReconnect(elapse uint16)
```
Set time that elapse between 2 reconnections attempt. By default there is no reconnection attempt if connection fails or connection is lost. Setting a value superior to 0 will enable that feature. Setting it to 0 will disable the feature.  
The value is in second.

```go
func (n *Node) SetMaxSocket(max uint8)
```
Set the maximum number of parallel socket connections. This is an optimization feature to maximize bandwith usage when streaming binary data. Binary packets are split across several parallel connections. Data is resequenced on arrival.  
Default value is 1. Maximum value is 255. Sweet spot is generaly 4. Too much connections might clutter the goroutine scheduler and cause leak.  

```go
func (n *Node) Serve()
```
Serve incoming messages either RPC request or RPC response. The method is blocking.

```go
func (n *Node) Close() error
```
Close connection. If reconnection is enable it will try to reconnect. You might want to disable reconnection before closing.

```go
func (n *Node) GetConnection() *Conn
```
Wait for a connection to be established and return a Conn object.

### type Conn
```go
type Conn struct {
	Header *Header
}
```
Conn object gives you access to the Header used during connection validation.
If parallel connections are allowed, this Header will be use to validate these connections. Modification to the Header can be done accordingly (ie: token update).

```go
func (c *Conn) OnDisconnect() chan bool
```
The returned channel will be closed when connection is closed.  
Because all receive operator of a channel will return when it close; you can effectively use this code in as many goroutine as needed:
```go
select {
case <-c.OnDisconnect():
	// something happen here
// any other concurrent channels can be added too.
}
```

```go
func (c *Conn) RemoteCall(name string, kwargs, reply interface{}) (pending PendingRequest, err error)
```
Make a remote call request.  
**reply** must be a pointer to an JSON encodable object initialized before sending the request.  
**name** is the name of the **Service** your are trying to use and its **method** name written in dotted syntax: **'MyService.MyFunc'**.  
**kwargs** is an object that can be JSON encoded that will be send together with the remote request.  
**error** if non **nil** will represent the error happened during sending the request.  
**pending** is an object that let you know when the **reply** value will be recieved and ready to use. See below for more details.

```go
func (c *Conn) RemoteStream(name string, kwargs interface{}) (stream StreamReceiver, err error)
```
Make a remote request and wait for stream of binary packets.  
**name** is the name of the **Service** your are trying to use and its **method** name written in dotted syntax: **'MyService.MyFunc'**.  
**kwargs** is an object that can be JSON encoded that will be send together with the remote request.  
**stream** is an object that let you handle receive binary packet. See below for more details.
Note that the service method signature in the other end must use as reply argument a **StreamSender** type interface in order to send binary packet.

```go
func (c *Conn) IsConnected() bool
```
Return true if still connected to remote end.

```go
func (c *Conn) Close() error
```
Close connection.

### type Header
Object type passed as argument to **OnHandshake()**. You may add properties to is on the client side, and read/validate properties on the server side.
The only required and default property is *'channel'* which is used to group parallel connections together.

```go
func (h *Header) Has(key string) bool
```
Test if a property exists.

```go
func (h *Header) Get(key string) interface{}
```
Return a property from the header. Use type assertion to convert the property to the desired type *(ie: header.Get("token").(string) )*.

```go
func (h *Header) Set(key string, data interface{})
```
Add a property to the header.
**Note** that the header is sent as **JSON**, for that reason sent data must be supported by the format.

```go
func (h *Header) Delete(key string)
```
Delete a property from the header.

### type PendingRequest
```go
type PendingRequest Interface {
	Error() error
	Wait() error
	WaitTimeout(time.Duration) error
	HasFailed() bool
	OnDone() chan bool
	IsDone() bool
}
```
Returned by **RemoteCall()** to know when **Reply()** is ready and if any error happened.  
**Error()** will be different than nil if an error occured.  
To know the state of the pending request, use either **OnDone()**, **Wait()** or **WaitTimeout()**.  
```go
func (p PendingRequest) OnDone() chan bool
```
The **OnDone()** return a channel that will be closed when **Reply()** is ready.  
Because all receive operator of a channel will return when it close; you can effectively use this code in as many goroutine as needed:
```go
select {
case <-p.OnDone():
	// something happen here
// any other concurrent channels can be added too.
}
```
**Wait()** and **WaitTimeout()** in the other hand are blocking.

```go
func (p PendingRequest) Wait() error
```
Will block until the *Reply()* is ready. If an error occured during th execution it will be returned, otherwise nil.

```go
func (p PendingRequest) WaitTimeout(t time.Duration) error
```
Like **Wait()** but will return **wsrpc.ErrTimeout** if timeout is reached before.

```go
func (p PendingRequest) HasFailed() bool
```
Return true if Error is different than nil. The return value is valid only if the pending request was completed.

### type StreamReceiver
Implement PendingRequest interface.
```go
type StreamReceiver interface {
	Error() error
	Wait() error
	WaitTimeout(time.Duration) error
	HasFailed() bool
	OnDone() chan bool
	IsDone() bool

	Receive() ([]byte, error)
	ReceiveFile(io.Writer) error
	Cancel() error
	Progress() (uint64, uint64)
}
```

```go
func (s StreamReceiver) Receive() ([]byte, error)
```
The method wait until the incoming binary data packet is received.
If bytes data is empty and err is nil, the stream has been closed.  
**Note** that if data is received but not read through either this method or **ReceiveFile()** data is cached on memory. Application might leak if not read in a timely fashion.  

```go
func (s StreamReceiver) ReceiveFile(io.Writer) error
```
Binary packets are writen directly into the *io.Writer* object. The method block until the stream is closed.  
This method is designed to be use in conjunction with **StreamSender.SendFile()** method on the other end.  
When data are received this way, the first packet sent is expected to be the payload encoded as a uint64 in big-endian for tracking purposes. **SendFile()** does that step automaticaly.  
The rest of the data is expected to be received by small chunks until the payload is reached.  
**Note** that if data is received but not read through either this method or **Receive()** data is cached on memory. Application might leak if not read in a timely fashion.  

```go
func (s StreamReceiver) Cancel() error
```
Cancel the current stream request by notifying the remote end.

```go
func (s StreamReceiver) Progress() (uint64, uint64)
```
Return the current progress when using **ReceiveFile()** method.  
**StreamSender.SendFile()** send data in chunks based on the length of bytes read from the *io.Reader* . It is that progression that is returned.
The first value is the number of chunks received, and the second value is the total number of expected chunks. The last number will be equal to zero until the payload is received.  
The chunks have a size of **wsrpc.MaxChunkSize** (or less for the last one).  
**wsrpc.PacketRequire(length uint64)** can be used to calculate the number of require packet to be received.

### type StreamSender
Implement PendingRequest interface.
```go
type StreamSender interface {
	Error() error
	Wait() error
	WaitTimeout(time.Duration) error
	HasFailed() bool
	OnDone() chan bool
	IsDone() bool

	Send([]byte) error
	SendFile(io.Reader, uint64) error
	End()
	Cancel() error
	Progress() (uint64, uint64)
}
```

```go
func (s StreamSender) Send(data []byte) error
```
Send the bytes to the remote end.
```go
func (s StreamSender) SendFile(src io.Reader, length uint64) error
```
Send *length* bytes from **io.Reader**. Block until everything was sent.
```go
func (s StreamSender) End()
```
Close the stream.
```go
func (s StreamSender) Cancel() error
```
Cancel the current stream request by notifying the remote end.
```go
func (s StreamSender) Progress() (uint64, uint64)
```
Return the current progress when using **SendFile()** method.  
**StreamSender.SendFile()** send data in chunks based on the length of bytes read from the *io.Reader* . It is that progression that is returned.
The first value is the number of chunks sent, and the second value is the total number of planned chunks.  
The chunks have a size of **wsrpc.MaxChunkSize** (or less for the last one).  
**wsrpc.PacketRequire(length uint64)** can be used to calculate the number of require packet to be sent over.
