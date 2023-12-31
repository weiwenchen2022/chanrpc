package chanrpc_test

import (
	"errors"
	"fmt"
	"log"

	"github.com/weiwenchen2022/chanrpc"
	"github.com/weiwenchen2022/chanrpc/chanpipe"

	pb "github.com/weiwenchen2022/chanrpc/helloworld"
)

type Args struct {
	A, B int
}

type Quotient struct {
	Quo, Rem int
}

type Arith int

func (*Arith) Multiply(args *Args, reply *int) error {
	*reply = args.A * args.B
	return nil
}

func (*Arith) Divide(args *Args, quo *Quotient) error {
	if args.B == 0 {
		return errors.New("divide by zero")
	}
	quo.Quo = args.A / args.B
	quo.Rem = args.A % args.B
	return nil
}

func Example() {
	arith := new(Arith)
	s := chanrpc.NewServer()
	s.Register(arith)

	c1, c2 := chanpipe.Pipe()
	go s.ServeChan(c2)

	client := chanrpc.NewClient(c1)

	// Synchronous call
	args := &Args{7, 8}
	var reply int
	err := client.Call("Arith.Multiply", args, &reply)
	if err != nil {
		log.Fatal("arith error:", err)
	}
	fmt.Printf("Arith: %d * %d = %d\n", args.A, args.B, reply)

	// Asynchronous call
	quotient := new(Quotient)
	divCall := client.Go("Arith.Divide", args, quotient, nil)
	replyCall := <-divCall.Done // will be equal to divCall
	// check errors, print, etc.
	fmt.Printf("Arith: %d / %d = %d, %d\n",
		args.A, args.B, replyCall.Reply.(*Quotient).Quo, replyCall.Reply.(*Quotient).Rem)

	// Output:
	// Arith: 7 * 8 = 56
	// Arith: 7 / 8 = 0, 7
}

type Greeter struct{}

func (*Greeter) SayHello(in *pb.HelloRequest, reply *pb.HelloReply) error {
	log.Printf("Received: %s", in.GetName())
	*reply = pb.HelloReply{Message: "Hello " + in.GetName()}
	return nil
}

func Example_helloworld() {
	s := chanrpc.NewServer()
	s.Register(&Greeter{})

	c1, c2 := chanpipe.Pipe()
	go s.ServeChan(c2)

	c := chanrpc.NewClient(c1)

	var reply any
	err := c.Call("Greeter.SayHello", &pb.HelloRequest{Name: "world"}, &reply)
	if err != nil {
		log.Fatalf("could not greet: %v", err)
	}

	// reply := r.(pb.HelloReply)
	fmt.Printf("Greeting: %s", reply.(*pb.HelloReply).GetMessage())

	// Output:
	// Greeting: Hello world
}
