// Copyright (C) 2019 Nick Rosbrook
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package vici

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	// Event listener channel was closed
	errChannelClosed = errors.New("vici: event listener channel closed")
)

type eventListener struct {
	*transport

	// Internal Context and CancelFunc used to stop the
	// listen loop.
	ctx    context.Context
	cancel context.CancelFunc

	// Event channel and the events it's listening for.
	ec chan Event

	// Lock events when registering and unregistering.
	mu     sync.Mutex
	events []string

	// Packet channel used to communicate event registration
	// results.
	pc   chan *packet
	perr chan error
}

// Event represents an event received by a Session sent from the
// charon daemon. It contains an associated Message and corresponds
// to one of the event types registered with Session.Listen.
type Event struct {
	// Name is the event type name as specified by the
	// charon server, such as "ike-updown" or "log".
	Name string

	// Message is the Message associated with this event.
	Message *Message

	// Timestamp holds the timestamp of when the client
	// received the event.
	Timestamp time.Time
}

func newEventListener(t *transport) *eventListener {
	ctx, cancel := context.WithCancel(context.Background())

	el := &eventListener{
		transport: t,
		ctx:       ctx,
		cancel:    cancel,
		ec:        make(chan Event, 16),
		pc:        make(chan *packet, 4),
		perr:      make(chan error, 1),
	}

	go el.listen()

	return el
}

// Close closes the event channel.
func (el *eventListener) Close() error {
	// This call interacts with charon, so get it
	// done first. Then, we can stop the listen
	// goroutine.
	if err := el.unregisterEvents(nil, true); err != nil {
		return err
	}

	// Cancel the event listener context, thus
	// stopping the listen goroutine, and wait
	// for the destroy context to be done.
	el.cancel()
	el.conn.Close()

	return nil
}

// listen is responsible for receiving all packets from the daemon. This includes
// not only event packets, but event registration confirmations/errors.
func (el *eventListener) listen() {
	// Clean up the event channel when this loop is closed. This
	// ensures any active NextEvent callers return.
	defer close(el.ec)

	for {
		select {
		case <-el.ctx.Done():
			// Event listener is closing...
			return

		default:
			// Try to read a packet...
		}

		p, err := el.recv()
		if err != nil {
			// If there is an error already buffered, that means there
			// was no eventTransportCommunicate caller to read it. The
			// buffer size is only 1, so flush before writing.
			select {
			case <-el.perr:
			default:
			}
			el.perr <- err

			// If we got EOF, then the event listener transport
			// has been closed (by a stopped daemon or otherwise),
			// and all subsequent calls to recv() will result in
			// the same error. Time to break out of the loop.
			//
			// See https://github.com/strongswan/govici/issues/24.
			if err == io.EOF {
				return
			}

			continue
		}

		ts := time.Now()

		switch p.ptype {
		case pktEvent:
			e := Event{
				Name:      p.name,
				Message:   p.msg,
				Timestamp: ts,
			}

			el.ec <- e

		// These SHOULD be in response to event registration
		// requests from the event listener. Forward them over
		// the packet channel.
		case pktEventConfirm, pktEventUnknown:
			el.pc <- p
		}
	}
}

func (el *eventListener) nextEvent(ctx context.Context) (Event, error) {
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()

	case e, ok := <-el.ec:
		if !ok {
			return Event{}, errChannelClosed
		}

		return e, nil
	}
}

func (el *eventListener) registerEvents(events []string) error {
	el.mu.Lock()
	defer el.mu.Unlock()

	for _, event := range events {
		// Check if the event is already registered.
		exists := false

		for _, registered := range el.events {
			if event == registered {
				exists = true
				// Break out of the inner loop, this
				// event is already registered.
				break
			}
		}

		// Check the next event given...
		if exists {
			continue
		}

		if err := el.eventRegisterUnregister(event, true); err != nil {
			return err
		}

		// Add the event to the list of registered events.
		el.events = append(el.events, event)
	}

	return nil
}

func (el *eventListener) unregisterEvents(events []string, all bool) error {
	el.mu.Lock()
	defer el.mu.Unlock()

	if all {
		events = el.events
	}

	for _, e := range events {
		if err := el.eventRegisterUnregister(e, false); err != nil {
			return err
		}

		for i, registered := range el.events {
			if e != registered {
				continue
			}

			el.events = append(el.events[:i], el.events[i+1:]...)

			// Break from the inner loop, we found the event
			// in the list of registered events.
			break
		}
	}

	return nil
}

func (el *eventListener) eventRegisterUnregister(event string, register bool) error {
	ptype := pktEventRegister
	if !register {
		ptype = pktEventUnregister
	}

	p, err := el.eventTransportCommunicate(newPacket(ptype, event, nil))
	if err != nil {
		return err
	}

	if p.ptype == pktEventUnknown {
		return fmt.Errorf("%v: %v", errEventUnknown, event)
	}

	if p.ptype != pktEventConfirm {
		return fmt.Errorf("%v:%v", errUnexpectedResponse, p.ptype)
	}

	return nil
}

func (el *eventListener) eventTransportCommunicate(pkt *packet) (*packet, error) {
	// If an error was sent over this channel while a
	// transport communication was not active, flush
	// it out quick before sending the packet.
	//
	// The channel buffer is only 1, so if there is an
	// error buffered, it is the _only_ error buffered.
	select {
	case <-el.perr:
	default:
	}

	err := el.send(pkt)
	if err != nil {
		return nil, err
	}

	// After the packet is sent, rely on the listen loop
	// to communicate the response. Previously, the read
	// deadline here was set to 1 second. Because this logic
	// may prove fragile, add an extra second for cushion.
	select {
	case <-time.After(2 * time.Second):
		return nil, errTransport

	case err := <-el.perr:
		return nil, err

	case p := <-el.pc:
		return p, nil
	}
}
