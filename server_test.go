// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package chanrpc

import (
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weiwenchen2022/chanrpc/chanpipe"
)

var (
	newServer *Server
	newClient *Client
	newOnce   sync.Once
)

type Args struct {
	A, B int
}

type Reply struct {
	C int
}

type Arith int

// Some of Arith's methods have value args, some have pointer args. That's deliberate.

func (*Arith) Add(args Args, reply *Reply) error {
	reply.C = args.A + args.B
	return nil
}

func (*Arith) Mul(args *Args, reply *Reply) error {
	reply.C = args.A * args.B
	return nil
}

func (*Arith) Div(args Args, reply *Reply) error {
	if args.B == 0 {
		return errors.New("divide by zero")
	}
	reply.C = args.A / args.B
	return nil
}

func (*Arith) String(args *Args, reply *string) error {
	*reply = fmt.Sprintf("%d + %d = %d", args.A, args.B, args.A+args.B)
	return nil
}

func (*Arith) Scan(args string, reply *Reply) (err error) {
	_, err = fmt.Sscan(args, &reply.C)
	return
}

func (*Arith) Error(*Args, *Reply) error {
	panic("ERROR")
}

func (*Arith) SleepMilli(args *Args, reply *Reply) error {
	time.Sleep(time.Duration(args.A) * time.Millisecond)
	return nil
}

type hidden int

func (*hidden) Exported(args Args, reply *Reply) error {
	reply.C = args.A + args.B
	return nil
}

type Embed struct {
	hidden
}

type BuiltinTypes struct{}

func (BuiltinTypes) Map(args *Args, reply *map[int]int) error {
	(*reply)[args.A] = args.B
	return nil
}

func (BuiltinTypes) Slice(args *Args, reply *[]int) error {
	*reply = append(*reply, args.A, args.B)
	return nil
}

func (BuiltinTypes) Array(args *Args, reply *[2]int) error {
	(*reply)[0] = args.A
	(*reply)[1] = args.B
	return nil
}

func makePipe() (c1, c2 chanpipe.Chan) {
	c1, c2 = chanpipe.Pipe()
	return
}

func startNewServer() {
	newServer = NewServer()
	newServer.Register(new(Arith))
	newServer.Register(new(Embed))
	newServer.RegisterName("net.rpc.Arith", new(Arith))
	newServer.RegisterName("newServer.Arith", new(Arith))
	newServer.Register(BuiltinTypes{})

	c1, c2 := makePipe()
	log.Println("NewServer test RPC server listening on", c2.LocalAddr())
	go newServer.ServeChan(c2)
	newClient = NewClient(c1)
}

func TestRPC(t *testing.T) {
	newOnce.Do(startNewServer)
	testRPC(t)
	testNewServerRPC(t)
}

func testRPC(t *testing.T) {
	// Synchronous calls
	args := &Args{7, 8}
	reply := new(Reply)
	err := newClient.Call("Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err)
	}
	if args.A+args.B != reply.C {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}

	// Methods exported from unexported embedded structs
	args = &Args{7, 0}
	reply = new(Reply)
	err = newClient.Call("Embed.Exported", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err)
	}
	if args.A+args.B != reply.C {
		t.Errorf("Add: expected %d got %d", reply.C, args.A+args.B)
	}

	// Nonexistent method
	args = &Args{7, 0}
	reply = new(Reply)
	err = newClient.Call("Arith.BadOperation", args, reply)
	// expect an error
	if err == nil {
		t.Error("BadOperation: expected error")
	} else if !strings.HasPrefix(err.Error(), "chanrpc: can't find method ") {
		t.Errorf("BadOperation: expected can't find method error; got %q", err)
	}

	// Unknown service
	args = &Args{7, 8}
	reply = new(Reply)
	err = newClient.Call("Arith.Unknown", args, reply)
	if err == nil {
		t.Error("expected error calling unknown service")
	} else if !strings.Contains(err.Error(), "method") {
		t.Error("expected error about method; got", err)
	}

	// Out of order.
	args = &Args{7, 8}
	mulReply := new(Reply)
	mulCall := newClient.Go("Arith.Mul", args, mulReply, nil)
	addReply := new(Reply)
	addCall := newClient.Go("Arith.Add", args, addReply, nil)

	addCall = <-addCall.Done
	if addCall.Error != nil {
		t.Errorf("Add: expected no error but got string %q", addCall.Error)
	}
	if args.A+args.B != addReply.C {
		t.Errorf("Add: expected %d got %d", addReply.C, args.A+args.B)
	}

	mulCall = <-mulCall.Done
	if mulCall.Error != nil {
		t.Errorf("Mul: expected no error but got string %q", mulCall.Error)
	}
	if args.A*args.B != mulReply.C {
		t.Errorf("Mul: expected %d got %d", mulReply.C, args.A*args.B)
	}

	// Error test
	args = &Args{7, 0}
	reply = new(Reply)
	err = newClient.Call("Arith.Div", args, reply)
	// expect an error: zero divide
	if err == nil {
		t.Error("Div: expected error")
	} else if err.Error() != "divide by zero" {
		t.Error("Div: expected divide by zero error; got", err)
	}

	// Bad type.
	reply = new(Reply)
	err = newClient.Call("Arith.Add", reply, reply) // args, reply would be the correct thing to use
	if err == nil {
		t.Error("expected error calling Arith.Add with wrong arg type")
	} else if !strings.Contains(err.Error(), "type") {
		t.Error("expected error about type; got", err)
	}

	// Non-struct argument
	const Val = 12345
	str := fmt.Sprint(Val)
	reply = new(Reply)
	err = newClient.Call("Arith.Scan", &str, reply)
	if err != nil {
		t.Errorf("Scan: expected no error but got string %q", err)
	} else if Val != reply.C {
		t.Errorf("Scan: expected %d got %d", Val, reply.C)
	}

	// Non-struct reply
	args = &Args{27, 35}
	str = ""
	err = newClient.Call("Arith.String", args, &str)
	if err != nil {
		t.Errorf("String: expected no error but got string %q", err)
	}
	expect := fmt.Sprintf("%d + %d = %d", args.A, args.B, args.A+args.B)
	if expect != str {
		t.Errorf("String: expected %q got %q", expect, str)
	}

	args = &Args{7, 8}
	reply = new(Reply)
	err = newClient.Call("Arith.Mul", args, reply)
	if err != nil {
		t.Errorf("Mul: expected no error but got string %q", err)
	}
	if args.A*args.B != reply.C {
		t.Errorf("Mul: expected %d got %d", args.A*args.B, reply.C)
	}

	// ServiceName contain "." character
	args = &Args{7, 8}
	reply = new(Reply)
	err = newClient.Call("net.rpc.Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err)
	}
	if args.A+args.B != reply.C {
		t.Errorf("Add: expected %d got %d", args.A+args.B, reply.C)
	}
}

func testNewServerRPC(t *testing.T) {
	// Synchronous calls
	args := &Args{7, 8}
	reply := new(Reply)

	err := newClient.Call("newServer.Arith.Add", args, reply)
	if err != nil {
		t.Errorf("Add: expected no error but got string %q", err)
	}
	if args.A+args.B != reply.C {
		t.Errorf("Add: expected %d got %d", args.A+args.B, reply.C)
	}
}

func TestBuiltinTypes(t *testing.T) {
	newOnce.Do(startNewServer)

	// Map
	args := &Args{7, 8}
	replyMap := make(map[int]int)
	err := newClient.Call("BuiltinTypes.Map", args, &replyMap)
	if err != nil {
		t.Errorf("Map: expected no error but got string %q", err)
	}
	if args.B != replyMap[args.A] {
		t.Errorf("Map: expected %d got %d", args.B, replyMap[args.A])
	}

	// Slice
	args = &Args{7, 8}
	var replySlice []int
	err = newClient.Call("BuiltinTypes.Slice", args, &replySlice)
	if err != nil {
		t.Errorf("Slice: expected no error but got string %q", err)
	}
	if e := []int{args.A, args.B}; fmt.Sprintf("%#v", e) != fmt.Sprintf("%#v", replySlice) {
		t.Errorf("Slice: expected %#v got %#v", e, replySlice)
	}

	// Array
	args = &Args{7, 8}
	var replyArray [2]int
	err = newClient.Call("BuiltinTypes.Array", args, &replyArray)
	if err != nil {
		t.Errorf("Array: expected no error but got string %q", err)
	}
	if e := [2]int{args.A, args.B}; e != replyArray {
		t.Errorf("Array: expected %v got %v", e, replyArray)
	}
}

type ReplyNotPointer int
type ArgNotPublic int
type ReplyNotPublic int
type NeedsPtrType int
type local struct{}

func (*ReplyNotPointer) ReplyNotPointer(*Args, Reply) error { return nil }

func (*ArgNotPublic) ArgNotPublic(*local, *Reply) error { return nil }

func (*ReplyNotPublic) ReplyNotPublic(*Args, *local) error { return nil }

func (*NeedsPtrType) NeedsPtrType(*Args, *Reply) error { return nil }

// Check that registration handles lots of bad methods and a type with no suitable methods.
func TestRegistrationError(t *testing.T) {
	s := NewServer()
	err := s.Register(new(ReplyNotPointer))
	if err == nil {
		t.Error("expected error registering ReplyNotPointer")
	}
	err = s.Register(new(ArgNotPublic))
	if err == nil {
		t.Error("expected error registering ArgNotPublic")
	}
	err = s.Register(new(ReplyNotPublic))
	if err == nil {
		t.Error("expected error registering ReplyNotPublic")
	}
	err = s.Register(NeedsPtrType(0))
	if err == nil {
		t.Error("expected error registering NeedsPtrType")
	} else if !strings.Contains(err.Error(), "pointer") {
		t.Error("expected hint when registering NeedsPtrType")
	}
}

type WriteFailCodec int

func (WriteFailCodec) WriteRequest(*Request) error {
	// the panic caused by this error used to not unlock a lock.
	return errors.New("fail")
}

func (WriteFailCodec) ReadResponse(*Response) error {
	select {}
}

func (WriteFailCodec) ReadResponseBody(any) error {
	select {}
}

func (WriteFailCodec) Close() error {
	return nil
}

func TestSendDeadlock(t *testing.T) {
	client := newClientWithCodec(WriteFailCodec(0))
	defer client.Close()

	done := make(chan bool)
	go func() {
		testSendDeadlock(client)
		testSendDeadlock(client)
		done <- true
	}()
	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock")
	}
}

func testSendDeadlock(client *Client) {
	defer func() { _ = recover() }()
	args := &Args{7, 8}
	reply := new(Reply)
	client.Call("Arith.Add", args, reply)
}

func countMallocs(t *testing.T) float64 {
	newOnce.Do(startNewServer)

	args := &Args{7, 8}
	reply := new(Reply)
	return testing.AllocsPerRun(100, func() {
		err := newClient.Call("Arith.Add", args, reply)
		if err != nil {
			t.Errorf("Add: expected no error but got string %q", err)
		}
		if args.A+args.B != reply.C {
			t.Errorf("Add: expected %d got %d", args.A+args.B, reply.C)
		}
	})
}

func TestCountMallocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping malloc count in short mode")
	}
	if runtime.GOMAXPROCS(0) > 1 {
		t.Skip("skipping; GOMAXPROCS>1")
	}
	fmt.Printf("mallocs per rpc round trip: %v\n", countMallocs(t))
}

type sendCrasher struct {
	chanpipe.Chan

	done chan bool
}

func (sendCrasher) Close() error {
	return nil
}

func (s *sendCrasher) Recv() (any, error) {
	<-s.done
	return nil, io.EOF
}

func (sendCrasher) Send(any) error {
	return errors.New("fake send failure")
}

func TestClientSendError(t *testing.T) {
	s := &sendCrasher{done: make(chan bool)}
	c := NewClient(s)
	defer c.Close()

	if err := c.Call("foo", 1, new(bool)); err == nil {
		t.Fatal("expected error")
	} else if err.Error() != "fake send failure" {
		t.Error("unexpected value of error:", err)
	}
	s.done <- true
}

func TestErrorAfterClientClose(t *testing.T) {
	newOnce.Do(startNewServer)
	err := newClient.Close()
	if err != nil {
		t.Fatal("close error:", err)
	}
	t.Cleanup(startNewServer)

	err = newClient.Call("Arith.Add", &Args{7, 9}, new(Reply))
	if ErrShutdown != err {
		t.Errorf("Forever: expected ErrShutdown got %v", err)
	}
}

// Tests the fix to issue 11221. Without the fix, this loops forever or crashes.
func TestServeChanExitAfterClientClose(t *testing.T) {
	newServer := NewServer()
	newServer.Register(new(Arith))
	newServer.RegisterName("net.rpc.Arith", new(Arith))
	newServer.RegisterName("newServer.Arith", new(Arith))

	cli, srv := makePipe()
	cli.Close()
	newServer.ServeChan(srv)
}

func benchmarkEndToEnd(b *testing.B) {
	newOnce.Do(startNewServer)

	// Synchronous calls
	args := &Args{7, 8}
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		reply := new(Reply)
		for pb.Next() {
			err := newClient.Call("Arith.Add", args, reply)
			if err != nil {
				b.Fatalf("rpc error: Add: expected no error but got string %q", err)
			}
			if args.A+args.B != reply.C {
				b.Fatalf("rpc error: Add: expected %d got %d", args.A+args.B, reply.C)
			}
		}
	})
}

func benchmarkEndToEndAsync(b *testing.B) {
	if b.N == 0 {
		return
	}

	const MaxConcurrentCalls = 100
	newOnce.Do(startNewServer)

	// Asynchronous calls
	args := &Args{7, 8}
	procs := 4 * runtime.GOMAXPROCS(-1)
	var send atomic.Int32
	send.Store(int32(b.N))
	var recv atomic.Int32
	recv.Store(int32(b.N))
	var wg sync.WaitGroup
	wg.Add(procs)
	gate := make(chan bool, MaxConcurrentCalls)
	res := make(chan *Call, MaxConcurrentCalls)
	b.ResetTimer()

	for p := 0; p < procs; p++ {
		go func() {
			for send.Add(-1) >= 0 {
				gate <- true
				reply := new(Reply)
				newClient.Go("Arith.Add", args, reply, res)
			}
		}()
		go func() {
			for call := range res {
				A := call.Args.(*Args).A
				B := call.Args.(*Args).B
				C := call.Reply.(*Reply).C
				if A+B != C {
					b.Errorf("incorrect reply: Add: expected %d got %d", A+B, C)
					return
				}
				<-gate
				if recv.Add(-1) == 0 {
					close(res)
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func BenchmarkEndToEnd(b *testing.B) {
	benchmarkEndToEnd(b)
}

func BenchmarkEndToEndAsync(b *testing.B) {
	benchmarkEndToEndAsync(b)
}
