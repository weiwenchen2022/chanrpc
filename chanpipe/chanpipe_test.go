// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package chanpipe_test

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	. "github.com/weiwenchen2022/chanrpc/chanpipe"
)

func checkSend(t *testing.T, w Sender, a any, c chan int) {
	if err := w.Send(a); err != nil {
		t.Errorf("send: %v", err)
	}
	c <- 0
}

// Test a single recv/send pair.
func TestPipe1(t *testing.T) {
	t.Parallel()

	c := make(chan int)
	c1, c2 := Pipe()
	go checkSend(t, c2, "hello, world", c)

	if a, err := c1.Recv(); err != nil {
		t.Errorf("recv: %v", err)
	} else if a != "hello, world" {
		t.Errorf("bad recv: got %#v", a)
	}
	<-c
	c1.Close()
	c2.Close()
}

type pipeTest struct {
	async bool
	err   error
}

func (p pipeTest) String() string {
	return fmt.Sprintf("async=%t err=%v", p.async, p.err)
}

var pipeTests = []pipeTest{
	{true, nil},
	{}, // all zero values
}

func delayClose(t *testing.T, cl io.Closer, ch chan int, tt pipeTest) {
	time.Sleep(1 * time.Millisecond)

	if err := cl.Close(); err != nil {
		t.Errorf("delayClose: %v", err)
	}
	ch <- 0
}

func TestPipeReadClose(t *testing.T) {
	t.Parallel()

	for _, tt := range pipeTests {
		c := make(chan int, 1)
		c1, c2 := Pipe()
		if tt.async {
			go delayClose(t, c2, c, tt)
		} else {
			delayClose(t, c2, c, tt)
		}

		a, err := c1.Recv()
		<-c

		if a != nil {
			t.Errorf("recv on closed pipe returned %v", a)
		}

		wantErr := tt.err
		if wantErr == nil {
			wantErr = io.EOF
		}
		if wantErr != err {
			t.Errorf("recv from closed pipe: %v want %v", err, wantErr)
		}

		if err = c1.Close(); err != nil {
			t.Errorf("r.Close: %v", err)
		}
	}
}

// Test close on Recv side during Recv.
func TestPipeReadClose2(t *testing.T) {
	t.Parallel()

	c := make(chan int, 1)
	c1, _ := Pipe()
	go delayClose(t, c1, c, pipeTest{})

	a, err := c1.Recv()
	<-c
	if a != nil || io.ErrClosedPipe != err {
		t.Errorf("recv from closed pipe: %v, %v want nil, %v", a, err, io.ErrClosedPipe)
	}
}

// Test send after/before recviver close.

func TestPipeSendClose(t *testing.T) {
	t.Parallel()

	for _, tt := range pipeTests {
		c := make(chan int, 1)
		c1, c2 := Pipe()
		if tt.async {
			go delayClose(t, c1, c, tt)
		} else {
			delayClose(t, c1, c, tt)
		}

		err := c2.Send(nil)
		<-c
		expect := tt.err
		if expect == nil {
			expect = io.ErrClosedPipe
		}
		if expect != err {
			t.Errorf("send on closed pipe: %v want %v", err, expect)
		}
		if err := c2.Close(); err != nil {
			t.Errorf("w.Close: %v", err)
		}
	}
}

// Test close on Send side during Send.
func TestPipeSendClose2(t *testing.T) {
	t.Parallel()

	c := make(chan int, 1)
	_, c2 := Pipe()
	go delayClose(t, c2, c, pipeTest{})
	err := c2.Send(nil)
	<-c

	if io.ErrClosedPipe != err {
		t.Errorf("send to closed pipe: %v want %v", err, io.ErrClosedPipe)
	}
}

func TestSendNil(t *testing.T) {
	t.Parallel()

	c1, c2 := Pipe()
	go func() {
		_ = c2.Send(nil)
		c2.Close()
	}()

	var err error
	for err == nil {
		_, err = c1.Recv()
	}
	c1.Close()
}

func TestSendAfterSenderClose(t *testing.T) {
	t.Parallel()

	c1, c2 := Pipe()

	done := make(chan bool)
	var sendErr error
	go func() {
		err := c2.Send("hello")
		if err != nil {
			t.Errorf("got error: %q; expected none", err)
		}
		c2.Close()
		sendErr = c2.Send("world")
		done <- true
	}()

	buf := make([]string, 100)
	var n int
	for n = range buf {
		a, err := c1.Recv()
		if err != nil && io.EOF != err {
			t.Fatalf("got: %q; want: %q", err, io.EOF)
		}
		if err != nil {
			break
		}
		buf[n] = a.(string)
	}

	result := strings.Join(buf[:n], "")
	<-done

	if result != "hello" {
		t.Errorf("got: %q; want: %q", result, "hello")
	}
	if io.ErrClosedPipe != sendErr {
		t.Errorf("got: %q; want: %q", sendErr, io.ErrClosedPipe)
	}
}
