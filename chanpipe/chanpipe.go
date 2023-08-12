// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package chanpipe

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// pipeDeadline is an abstraction for handling timeouts.
type pipeDeadline struct {
	mu      sync.Mutex // Guards timer and cancel
	timer   *time.Timer
	cancnel chan struct{} // Must be non-nil
}

func makePipeDeadline() pipeDeadline {
	return pipeDeadline{cancnel: make(chan struct{})}
}

// set sets the point in time when the deadline will time out.
// A timeout event is signaled by closing the channel returned by waiter.
// Once a timeout has occurred, the deadline can be refreshed by specifying a
// t value in the future.
//
// A zero value for t prevents timeout.
func (d *pipeDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil && !d.timer.Stop() {
		<-d.cancnel // Wait for the timer callback to finish and close cancel
	}
	d.timer = nil

	// Time is zero, then there is no deadline.
	closed := isClosedChan(d.cancnel)
	if t.IsZero() {
		if closed {
			d.cancnel = make(chan struct{})
		}
		return
	}

	// Time in the future, setup a timer to cancel in the future.
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancnel = make(chan struct{})
		}
		d.timer = time.AfterFunc(dur, func() {
			close(d.cancnel)
		})
		return
	}

	// Time in the past, so close immediately.
	if !closed {
		close(d.cancnel)
	}
}

// wait returns a channel that is closed when the deadline is exceeded.
func (d *pipeDeadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancnel
}

func isClosedChan(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
	}
	return false
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

type pipe struct {
	// wrMu sync.Mutex // Serialize Write operations

	// Used by local Recv to interact with remote Send.
	recvCh <-chan any

	// Used by local Send to interact with remote Recv.
	sendCh chan<- any

	once       sync.Once // Protects closing localDone
	localDone  chan struct{}
	remoteDone <-chan struct{}

	recvDeadline pipeDeadline
	sendDeadline pipeDeadline
}

func (*pipe) LocalAddr() net.Addr  { return pipeAddr{} }
func (*pipe) RemoteAddr() net.Addr { return pipeAddr{} }

func (p *pipe) Recv() (any, error) {
	a, err := p.recv()
	if err != nil && io.EOF != err && io.ErrClosedPipe != err {
		err = &net.OpError{Op: "recv", Net: "pipe", Err: err}
	}
	return a, err
}

func (p *pipe) recv() (a any, err error) {
	select {
	case <-p.localDone:
		return nil, io.ErrClosedPipe
	case <-p.remoteDone:
		return nil, io.EOF
	case <-p.recvDeadline.wait():
		return nil, os.ErrDeadlineExceeded
	default:
	}

	select {
	case <-p.localDone:
		return nil, io.ErrClosedPipe
	case <-p.remoteDone:
		return nil, io.EOF
	case <-p.recvDeadline.wait():
		return nil, os.ErrDeadlineExceeded
	case a = <-p.recvCh:
		return a, nil
	}
}

func (p *pipe) Send(a any) error {
	err := p.send(a)
	if err != nil && io.ErrClosedPipe != err {
		err = &net.OpError{Op: "send", Net: "pipe", Err: err}
	}
	return err
}

func (p *pipe) send(a any) (err error) {
	select {
	case <-p.localDone:
		return io.ErrClosedPipe
	case <-p.remoteDone:
		return io.ErrClosedPipe
	case <-p.sendDeadline.wait():
		return os.ErrDeadlineExceeded
	default:
	}

	// p.wrMu.Lock() // Ensure entirety of a is written together
	// defer p.wrMu.Unlock()

	select {
	case <-p.localDone:
		return io.ErrClosedPipe
	case <-p.remoteDone:
		return io.ErrClosedPipe
	case <-p.sendDeadline.wait():
		return os.ErrDeadlineExceeded
	case p.sendCh <- a:
	}
	return nil
}

func (p *pipe) SetDeadline(t time.Time) error {
	if err := p.SetRecvDeadline(t); err != nil {
		return err
	}
	return p.SetSendDeadline(t)
}

func (p *pipe) SetRecvDeadline(t time.Time) error {
	if isClosedChan(p.localDone) || isClosedChan(p.remoteDone) {
		return io.ErrClosedPipe
	}
	p.recvDeadline.set(t)
	return nil
}

func (p *pipe) SetSendDeadline(t time.Time) error {
	if isClosedChan(p.localDone) || isClosedChan(p.remoteDone) {
		return io.ErrClosedPipe
	}
	p.sendDeadline.set(t)
	return nil
}

func (p *pipe) Close() error {
	p.once.Do(func() { close(p.localDone) })
	return nil
}

// Pipe creates a synchronous, in-memory, full duplex
// network connection; both ends implement the Conn interface.
// Reads on one end are matched with writes on the other,
// copying data directly between the two; there is no internal
// buffering.
func Pipe() (Chan, Chan) {
	cb1 := make(chan any)
	cb2 := make(chan any)
	done1 := make(chan struct{})
	done2 := make(chan struct{})

	p1 := &pipe{
		recvCh:    cb1,
		sendCh:    cb2,
		localDone: done1, remoteDone: done2,
		recvDeadline: makePipeDeadline(),
		sendDeadline: makePipeDeadline(),
	}
	p2 := &pipe{
		recvCh:    cb2,
		sendCh:    cb1,
		localDone: done2, remoteDone: done1,
		recvDeadline: makePipeDeadline(),
		sendDeadline: makePipeDeadline(),
	}
	return p1, p2
}

// Chan is a generic value-oriented channel.
//
// Multiple goroutines may invoke methods on a Chan simultaneously.
type Chan interface {
	// Recv recives value from the channel.
	// Recv can be made to time out and return an error after a fixed
	// time limit; see SetDeadline and SetRecvDeadline.
	Recv() (any, error)

	// Send sends data to the channel.
	// Send can be made to time out and return an error after a fixed
	// time limit; see SetDeadline and SetSendDeadline.
	Send(any) error

	// Close closes the channel.
	// Any blocked Recv or Send operations will be unblocked and return errors.
	Close() error

	// LocalAddr returns the local channel address, if known.
	LocalAddr() net.Addr

	// RemoteAddr returns the remote channel address, if known.
	RemoteAddr() net.Addr

	// SetDeadline sets the recv and send deadlines associated
	// with the channel. It is equivalent to calling both
	// SetRecvDeadline and SetSendDeadline.
	//
	// A deadline is an absolute time after which I/O operations
	// fail instead of blocking. The deadline applies to all future
	// and pending I/O, not just the immediately following call to
	// Recv or Send. After a deadline has been exceeded, the
	// connection can be refreshed by setting a deadline in the future.
	//
	// If the deadline is exceeded a call to Recv or Send or to other
	// I/O methods will return an error that wraps os.ErrDeadlineExceeded.
	// This can be tested using errors.Is(err, os.ErrDeadlineExceeded).
	// The error's Timeout method will return true, but note that there
	// are other possible errors for which the Timeout method will
	// return true even if the deadline has not been exceeded.
	//
	// An idle timeout can be implemented by repeatedly extending
	// the deadline after successful Recv or Send calls.
	//
	// A zero value for t means I/O operations will not time out.
	SetDeadline(t time.Time) error

	// SetRecvDeadline sets the deadline for future Recv calls
	// and any currently-blocked Recv call.
	// A zero value for t means Recv will not time out.
	SetRecvDeadline(t time.Time) error

	// SetSendDeadline sets the deadline for future Send calls
	// and any currently-blocked Send call.
	// Even if send times out, it may return n > 0, indicating that
	// some of the data was successfully sent.
	// A zero value for t means Send will not time out.
	SetSendDeadline(t time.Time) error
}

// Sender is the interface that wraps the basic Send method.
type Sender interface {
	Send(any) error
}

// Receiver is the interface that wraps the basic Recv method.
type Receiver interface {
	Recv() (any, error)
}
