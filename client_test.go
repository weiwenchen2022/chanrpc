// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package chanrpc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type shutdownCodec struct {
	responded chan int
	closed    bool
}

func (*shutdownCodec) WriteRequest(*Request) error { return nil }
func (c *shutdownCodec) ReadResponse(*Response) error {
	c.responded <- 1
	return errors.New("shutdownCodec ReadResponseHeader")
}
func (c *shutdownCodec) Close() error {
	c.closed = true
	return nil
}

func TestCloseCodec(t *testing.T) {
	codec := &shutdownCodec{responded: make(chan int)}
	client := newClientWithCodec(codec)
	<-codec.responded
	client.Close()
	if !codec.closed {
		t.Error("client.Close did not close codec")
	}
}

// Test that errors in gob shut down the connection. Issue 7689.

type R struct {
	msg []byte // Not exported, so R does not work with gob.
}

type S struct{}

func (*S) Recv(nul *struct{}, reply *R) error {
	*reply = R{[]byte("foo")}
	return nil
}

func TestReflectError(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("no error")
		} else if !strings.Contains(r.(error).Error(), "not assignable to") {
			t.Fatal("expected `not assignable to', got", r)
		}
	}()

	s := NewServer()
	s.Register(new(S))

	cli, srv := makePipe()
	go s.ServeChan(srv)

	client := NewClient(cli)
	defer client.Close()

	var reply Reply
	err := client.Call("S.Recv", &struct{}{}, &reply)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%#v\n", reply)
}
