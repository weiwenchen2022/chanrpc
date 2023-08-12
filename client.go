// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package chanrpc

import (
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"sync"

	"github.com/weiwenchen2022/chanrpc/chanpipe"
)

// ServerError represents an error that has been returned from
// the remote side of the RPC channel.
type ServerError string

func (e ServerError) Error() string {
	return string(e)
}

var ErrShutdown = shutdownError{}

type shutdownError struct{}

func (shutdownError) Error() string { return "connection is shut down" }

// Call represents an active RPC.
type Call struct {
	ServiceMethod string     // The name of the service and method to call.
	Args          any        // The argument to the function (*struct).
	Reply         any        // The reply from the function (*struct).
	Error         error      // After completion, the error status.
	Done          chan *Call // Receives *Call when Go is complete.
}

// Client represents an RPC Client.
// There may be multiple outstanding Calls associated
// with a single Client, and a Client may be used by
// multiple goroutines simultaneously.
type Client struct {
	codec ClientCodec

	reqMutex sync.Mutex // protects following
	request  Request

	mutex    sync.Mutex // protects following
	seq      uint64
	pending  map[uint64]*Call
	closing  bool // user has called Close
	shutdown bool // server has told us to stop
}

// A ClientCodec implements sending of RPC requests and
// receiving of RPC responses for the client side of an RPC session.
// The client calls WriteRequest to send a request to the channel
// and calls ReadResponse to receive responses.
// The client calls Close when finished with the
// channel.
// See NewClient's comment for information about concurrent access.
type ClientCodec interface {
	WriteRequest(*Request) error
	ReadResponse(*Response) error

	Close() error
}

func (client *Client) send(call *Call) {
	client.reqMutex.Lock()
	defer client.reqMutex.Unlock()

	// Register this call.
	client.mutex.Lock()
	if client.shutdown || client.closing {
		client.mutex.Unlock()
		call.Error = ErrShutdown
		call.done()
		return
	}
	seq := client.seq
	client.seq++
	client.pending[seq] = call
	client.mutex.Unlock()

	// Encode and send the request.
	client.request.Seq = seq
	client.request.ServiceMethod = call.ServiceMethod
	client.request.Args = call.Args
	err := client.codec.WriteRequest(&client.request)
	if err != nil {
		client.mutex.Lock()
		call = client.pending[seq]
		delete(client.pending, seq)
		client.mutex.Unlock()
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

func (client *Client) input() {
	var (
		err      error
		response Response
	)

	for err == nil {
		response = Response{}
		err = client.codec.ReadResponse(&response)
		if err != nil {
			break
		}
		seq := response.Seq
		client.mutex.Lock()
		call := client.pending[seq]
		delete(client.pending, seq)
		client.mutex.Unlock()

		switch {
		case call == nil:
		case response.Error != "":
			call.Error = ServerError(response.Error)
			call.done()
		default:
			// Decode the reply value.
			err = func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("%v", r)
					}
				}()
				replyv := reflect.ValueOf(response.Reply).Elem()
				reflect.ValueOf(call.Reply).Elem().Set(replyv)
				return nil
			}()
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}
	}

	// Terminate pending calls.
	client.reqMutex.Lock()
	client.mutex.Lock()
	client.shutdown = true
	closing := client.closing
	if io.EOF == err {
		if closing {
			err = ErrShutdown
		} else {
			err = io.ErrUnexpectedEOF
		}
	}

	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
	client.mutex.Unlock()
	client.reqMutex.Unlock()

	if debugLog && io.EOF != err && !closing {
		log.Println("chanrpc: client protocol error:", err)
	}
}

func (call *Call) done() {
	select {
	default:
		// We don't want to block here. It is the caller's responsibility to make
		// sure the channel has enough buffer space. See comment in Go().
		if debugLog {
			log.Println("chanrpc: discarding Call reply due to insufficient Done chan capacity")
		}
	case call.Done <- call:
		// ok
	}
}

// NewClient returns a new Client to handle requests to the
// set of services at the other end of the channel.
//
// The send and receive halves of the channel are serialized independently,
// so no interlocking is required. However each half may be accessed
// concurrently so the implementation of conn should protect against
// concurrent receives or concurrent sends.
func NewClient(ch chanpipe.Chan) *Client {
	client := &chanClientCodec{ch}
	return newClientWithCodec(client)
}

// newClientWithCodec is like NewClient but uses the specified
// codec to encode requests and decode responses.
func newClientWithCodec(codec ClientCodec) *Client {
	client := &Client{
		codec:   codec,
		pending: make(map[uint64]*Call),
	}
	go client.input()
	return client
}

type chanClientCodec struct {
	ch chanpipe.Chan
}

func (c *chanClientCodec) WriteRequest(r *Request) error {
	if err := c.ch.Send(*r); err != nil {
		return err
	}
	return nil
}

func (c *chanClientCodec) ReadResponse(r *Response) error {
	a, err := c.ch.Recv()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	*r = a.(Response)
	return nil
}

func (c *chanClientCodec) Close() error {
	return c.ch.Close()
}

// Close calls the underlying codec's Close method. If the connection is already
// shutting down, ErrShutdown is returned.
func (client *Client) Close() error {
	client.mutex.Lock()
	if client.closing {
		client.mutex.Unlock()
		return ErrShutdown
	}

	client.closing = true
	client.mutex.Unlock()
	return client.codec.Close()
}

// Go invokes the function asynchronously. It returns the Call structure representing
// the invocation. The done channel will signal when the call is complete by returning
// the same Call object. If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
func (client *Client) Go(serviceMethod string, args, reply any, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 1) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel. If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("chanrpc: done channel is unbuffered")
		}
	}

	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	client.send(call)
	return call
}

// Call invokes the named function, waits for it to complete, and returns its error status.
func (client *Client) Call(serviceMethod string, args, reply any) error {
	call := <-client.Go(serviceMethod, args, reply, make(chan *Call, 1)).Done
	return call.Error
}
