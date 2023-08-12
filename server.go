// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Package chanrpc provides access to the exported methods of an object across a
[chanpipe.Chan].  A server registers an object, making it visible
as a service with the name of the type of the object.  After registration, exported
methods of the object will be accessible remotely.  A server may register multiple
objects (services) of different types but it is an error to register multiple
objects of the same type.

Only methods that satisfy these criteria will be made available for remote access;
other methods will be ignored:

  - the method's type is exported.
  - the method is exported.
  - the method has two arguments, both exported (or builtin) types.
  - the method's second argument is a pointer.
  - the method has return type error.

In effect, the method must look schematically like

	func (t *T) MethodName(argType T1, replyType *T2) error

The method's first argument represents the arguments provided by the caller; the
second argument represents the result parameters to be returned to the caller.
The method's return value, if non-nil, is passed back as a string that the client
sees as if created by errors.New.  If an error is returned, the reply parameter
will not be sent back to the client.

The server may handle requests on a single Chan by calling ServeChan.

A client wishing to use the service establishes a connection and then invokes
NewClient on the connection. The Client object has two methods,
Call and Go, that specify the service and method to
call, a pointer containing the arguments, and a pointer to receive the result
parameters.

The Call method waits for the remote call to complete while the Go method
launches the call asynchronously and signals completion using the Call
structure's Done channel.

Here is a simple example.  A server wishes to export an object of type Arith:

	package server

	import "errors"

	type Args struct {
		A, B int
	}

	type Quotient struct {
		Quo, Rem int
	}

	type Arith int

	func (t *Arith) Multiply(args *Args, reply *int) error {
		*reply = args.A * args.B
		return nil
	}

	func (t *Arith) Divide(args *Args, quo *Quotient) error {
		if args.B == 0 {
			return errors.New("divide by zero")
		}
		quo.Quo = args.A / args.B
		quo.Rem = args.A % args.B
		return nil
	}

The server calls:

	arith := new(Arith)
	s := chanrpc.NewServer()
	s.Register(arith)
	c1, c2 := chanpipe.Pipe()
	go s.ServeChan(c2)

At this point, clients can see a service "Arith" with methods "Arith.Multiply" and
"Arith.Divide".  To invoke one, a client first constructs the Client:

	client := chanrcp.NewClient(c1)

Then it can make a remote call:

	// Synchronous call
	args := &server.Args{7,8}
	var reply int
	err = client.Call("Arith.Multiply", args, &reply)
	if err != nil {
		log.Fatal("arith error:", err)
	}
	fmt.Printf("Arith: %d * %d = %d", args.A, args.B, reply)

or

	// Asynchronous call
	quotient := new(Quotient)
	divCall := client.Go("Arith.Divide", args, quotient, nil)
	replyCall := <-divCall.Done	// will be equal to divCall
	// check errors, print, etc.

A server implementation will often provide a simple, type-safe wrapper for the
client.
*/
package chanrpc

import (
	"errors"
	"fmt"
	"go/token"
	"io"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/weiwenchen2022/chanrpc/chanpipe"
)

// Precompute the reflect type for error. Can't use error directly
// because Typeof takes an empty interface value. This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()

type methodType struct {
	method    reflect.Method
	ArgType   reflect.Type
	ReplyType reflect.Type
	numCalls  atomic.Uint64
}

type service struct {
	name   string                 // name of service
	rcvr   reflect.Value          // receiver of methods for the service
	typ    reflect.Type           // type of the receiver
	method map[string]*methodType // registered methods
}

// Request is sent before every RPC call. It is used internally
// but documented here as an aid to debugging, such as when analyzing
// network traffic.
type Request struct {
	ServiceMethod string // format: "Service.Method"
	Args          any    // The argument to the function (*struct).
	Seq           uint64 // sequence number chosen by client

	next *Request // for free list in Server
}

// Response is a header sent before every RPC return. It is used internally
// but documented here as an aid to debugging, such as when analyzing
// network traffic.
type Response struct {
	ServiceMethod string // echoes that of the Request
	Seq           uint64 // echoes that of the request
	Reply         any    // The reply from the function (*struct).
	Error         string // error, if any.

	next *Response // for free list in Server
}

// Server represents an RPC Server.
type Server struct {
	serviceMap sync.Map // map[string]*service

	reqLock sync.Mutex // protects freeReq
	freeReq *Request

	respLock sync.Mutex // protects freeResp
	freeResp *Response
}

// NewServer returns a new Server.
func NewServer() *Server {
	return &Server{}
}

// Is this type exported or a builtin?
func isExportedOrBuiltinType(t reflect.Type) bool {
	for reflect.Pointer == t.Kind() {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return token.IsExported(t.Name()) || t.PkgPath() == ""
}

// Register publishes in the server the set of methods of the
// receiver value that satisfy the following conditions:
//   - exported method of exported type
//   - two arguments, both of exported type
//   - the second argument is a pointer
//   - one return value, of type error
//
// It returns an error if the receiver is not an exported type or has
// no suitable methods. It also logs the error using package log.
// The client accesses each method using a string of the form "Type.Method",
// where Type is the receiver's concrete type.
func (server *Server) Register(rcvr any) error {
	return server.register(rcvr, "", false)
}

// RegisterName is like Register but uses the provided name for the type
// instead of the receiver's concrete type.
func (server *Server) RegisterName(name string, rcvr any) error {
	return server.register(rcvr, name, true)
}

// logRegisterError specifies whether to log problems during method registration.
// To debug registration, recompile the package with this set to true.
const logRegisterError = false

func (server *Server) register(rcvr any, name string, useName bool) error {
	s := &service{
		rcvr: reflect.ValueOf(rcvr),
		typ:  reflect.TypeOf(rcvr),
	}
	sname := name
	if !useName {
		sname = reflect.Indirect(s.rcvr).Type().Name()
	}
	if sname == "" {
		s := "chanrpc.Register: no service name for type " + s.typ.String()
		log.Print(s)
		return errors.New(s)
	}
	if !useName && !token.IsExported(sname) {
		s := "chanrpc.Register: type " + sname + " is not exported"
		log.Print(s)
		return errors.New(s)
	}
	s.name = sname

	// Install the methods
	s.method = suitableMethods(s.typ, logRegisterError)

	if len(s.method) == 0 {
		str := ""

		// To help the user, see if a pointer receiver would work.
		method := suitableMethods(reflect.PointerTo(s.typ), false)
		if len(method) != 0 {
			str = "chanrpc.Register: type " + sname + " has no exported methods of suitable type (hint: pass a pointer to value of that type)"
		} else {
			str = "chanrpc.Register: type " + sname + " has no exported methods of suitable type"
		}
		log.Print(str)
		return errors.New(str)
	}

	if _, dup := server.serviceMap.LoadOrStore(sname, s); dup {
		return errors.New("chanrpc: service already defined: " + sname)
	}
	return nil
}

// suitableMethods returns suitable Rpc methods of typ. It will log
// errors if logErr is true.
func suitableMethods(typ reflect.Type, logErr bool) map[string]*methodType {
	methods := make(map[string]*methodType, typ.NumMethod())
	for m := 0; m < typ.NumMethod(); m++ {
		method := typ.Method(m)
		mtype := method.Type
		mname := method.Name
		// Method must be exported.
		if !method.IsExported() {
			continue
		}
		// Method needs three ins: receiver, *args, *reply.
		if numIn := mtype.NumIn(); numIn != 3 {
			if logErr {
				log.Printf("chanrpc.Register: method %q has %d input parameters; needs exactly three\n", mname, numIn)
			}
			continue
		}
		// First arg need not be a pointer.
		argType := mtype.In(1)
		if !isExportedOrBuiltinType(argType) {
			if logErr {
				log.Printf("chanrpc.Register: argument type of method %q is not exported: %q\n", mname, argType)
			}
			continue
		}
		// Second arg must be a pointer.
		replyType := mtype.In(2)
		if reflect.Pointer != replyType.Kind() {
			if logErr {
				log.Printf("chanrpc.Register: reply type of method %q is not a pointer: %q\n", mname, replyType)
			}
			continue
		}
		// Reply type must be exported.
		if !isExportedOrBuiltinType(replyType) {
			if logErr {
				log.Printf("chanrpc.Register: reply type of method %q is not exported: %q\n", mname, replyType)
			}
			continue
		}
		// Method needs one out.
		if numOut := mtype.NumOut(); numOut != 1 {
			if logErr {
				log.Printf("chanrpc.Register: method %q has %d output parameters; needs exactly one\n", mname, numOut)
			}
			continue
		}
		// The return type of the method must be error.
		if returnType := mtype.Out(0); typeOfError != returnType {
			if logErr {
				log.Printf("chanrpc.Register: return type of method %q is %q, must be error\n", mname, returnType)
			}
			continue
		}
		methods[mname] = &methodType{method: method, ArgType: argType, ReplyType: replyType}
	}
	return methods
}

// A value sent as a placeholder for the server's response value when the server
// receives an invalid request. It is never decoded by the client since the Response
// contains an error when it is used.
var invalidRequest any

func (server *Server) sendResponse(sending *sync.Mutex, req *Request, reply any, codec ServerCodec, errmsg string) {
	resp := server.getResponse()
	defer server.freeResponse(resp)
	// Encode the response header
	resp.ServiceMethod = req.ServiceMethod
	if errmsg != "" {
		resp.Error = errmsg
		reply = invalidRequest
	}
	resp.Seq = req.Seq
	resp.Reply = reply
	sending.Lock()
	err := codec.WriteResponse(resp)
	sending.Unlock()
	if debugLog && err != nil {
		log.Println("chanrpc: writing response:", err)
	}
}

func (m *methodType) NumCalls() uint64 {
	return m.numCalls.Load()
}

func (s *service) call(server *Server, sending *sync.Mutex, wg *sync.WaitGroup, mtype *methodType, req *Request, argv, replyv reflect.Value, codec ServerCodec) {
	if wg != nil {
		defer wg.Done()
	}
	mtype.numCalls.Add(1)
	function := mtype.method.Func
	// Invoke the method, providing a new value for the reply.
	returnValues := function.Call([]reflect.Value{s.rcvr, argv, replyv})
	// The return value for the method is an error.
	errInter := returnValues[0].Interface()
	errmsg := ""
	if errInter != nil {
		errmsg = errInter.(error).Error()
	}
	server.sendResponse(sending, req, replyv.Interface(), codec, errmsg)
	server.freeRequest(req)
}

type chanServerCodec struct {
	ch     chanpipe.Chan
	closed bool
}

func (c *chanServerCodec) ReadRequestHeader(r *Request) (err error) {
	a, err := c.ch.Recv()
	if err != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	*r = a.(Request)
	return nil
}

func (c *chanServerCodec) WriteResponse(r *Response) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	if err = c.ch.Send(*r); err != nil {
		// Chan couldn't send the header. Should not happen, so if it does,
		// shut down the connection to signal that the connection is broken.
		log.Println("chanrpc: chan error sending response:", err)
		c.Close()
		return
	}
	return nil
}

func (c *chanServerCodec) Close() error {
	if c.closed {
		// Only call c.C.Close once; otherwise the semantics are undefined.
		return nil
	}
	c.closed = true
	return c.ch.Close()
}

// ServeChan runs the server on a single chan.
// ServeChan blocks, serving the channel until the channel close.
// The caller typically invokes ServeChan in a go statement.
// To use an alternate codec, use ServeCodec.
// See NewClient's comment for information about concurrent access.
func (server *Server) ServeChan(ch chanpipe.Chan) {
	srv := &chanServerCodec{
		ch: ch,
	}
	server.serveCodec(srv)
}

// serveCodec is like ServeChan but uses the specified codec to
// decode requests and encode responses.
func (server *Server) serveCodec(codec ServerCodec) {
	sending := new(sync.Mutex)
	wg := new(sync.WaitGroup)
	for {
		service, mtype, req, argv, replyv, keepReading, err := server.readRequest(codec)
		if err != nil {
			if debugLog && io.EOF != err {
				log.Println("chanrpc:", err)
			}
			if !keepReading {
				break
			}
			// send a response if we actually managed to read a header.
			if req != nil {
				server.sendResponse(sending, req, invalidRequest, codec, err.Error())
				server.freeRequest(req)
			}
			continue
		}
		wg.Add(1)
		go service.call(server, sending, wg, mtype, req, argv, replyv, codec)
	}
	// We've seen that there are no more requests.
	// Wait for responses to be sent before closing codec.
	wg.Wait()
	codec.Close()
}

func (server *Server) getRequest() *Request {
	server.reqLock.Lock()
	req := server.freeReq
	if req == nil {
		req = new(Request)
	} else {
		server.freeReq = req.next
		*req = Request{}
	}
	server.reqLock.Unlock()
	return req
}

func (server *Server) freeRequest(req *Request) {
	server.reqLock.Lock()
	req.next = server.freeReq
	server.freeReq = req
	server.reqLock.Unlock()
}

func (server *Server) getResponse() *Response {
	server.respLock.Lock()
	resp := server.freeResp
	if resp == nil {
		resp = new(Response)
	} else {
		server.freeResp = resp.next
		*resp = Response{}
	}
	server.respLock.Unlock()
	return resp
}

func (server *Server) freeResponse(resp *Response) {
	server.respLock.Lock()
	resp.next = server.freeResp
	server.freeResp = resp
	server.respLock.Unlock()
}

func (server *Server) readRequest(codec ServerCodec) (service *service, mtype *methodType, req *Request, argv, replyv reflect.Value, keepReading bool, err error) {
	service, mtype, req, keepReading, err = server.readRequestHeader(codec)
	if err != nil {
		return
	}

	// Decode the argument value.
	argIsValue := false // if true, need to indirect before calling.
	if reflect.Pointer == mtype.ArgType.Kind() {
		argv = reflect.New(mtype.ArgType.Elem())
	} else {
		argv = reflect.New(mtype.ArgType)
		argIsValue = true
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	// argv guaranteed to be a pointer now.
	argv.Elem().Set(reflect.Indirect(reflect.ValueOf(req.Args)))
	if argIsValue {
		argv = argv.Elem()
	}

	replyv = reflect.New(mtype.ReplyType.Elem())

	switch mtype.ReplyType.Elem().Kind() {
	case reflect.Map:
		replyv.Elem().Set(reflect.MakeMap(mtype.ReplyType.Elem()))
	}
	return
}

func (server *Server) readRequestHeader(codec ServerCodec) (svc *service, mtype *methodType, req *Request, keepReading bool, err error) {
	// Grab the request.
	req = server.getRequest()
	err = codec.ReadRequestHeader(req)
	if err != nil {
		server.freeRequest(req)
		req = nil
		if io.EOF == err || io.ErrUnexpectedEOF == err {
			return
		}
		err = errors.New("chanrpc: server cannot decode request: " + err.Error())
		return
	}

	// We read the header successfully. If we see an error now,
	// we can still recover and move on to the next request.
	keepReading = true

	dot := strings.LastIndex(req.ServiceMethod, ".")
	if dot < 0 {
		err = errors.New("chanrpc: service/method request ill-formed: " + req.ServiceMethod)
		return
	}
	serviceName := req.ServiceMethod[:dot]
	methodName := req.ServiceMethod[dot+1:]

	// Look up the request.
	svci, ok := server.serviceMap.Load(serviceName)
	if !ok {
		err = errors.New("chanrpc: can't find service " + req.ServiceMethod)
		return
	}
	svc = svci.(*service)
	mtype = svc.method[methodName]
	if mtype == nil {
		err = errors.New("chanrpc: can't find method " + req.ServiceMethod)
	}
	return
}

// A ServerCodec implements receiving of RPC requests and sending of
// RPC responses for the server side of an RPC session.
// The server calls ReadRequestHeader to receive requests
// from the channel, and it calls WriteResponse to
// send a response back. The server calls Close when finished with the
// channel.
// See NewClient's comment for information about concurrent access.
type ServerCodec interface {
	ReadRequestHeader(*Request) error
	WriteResponse(*Response) error

	// Close can be called multiple times and must be idempotent.
	Close() error
}
