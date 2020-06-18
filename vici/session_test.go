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
	"flag"
	"log"
	"net"
	"testing"
	"time"
)

func mockCharon(ctx context.Context) net.Conn {
	client, srvr := net.Pipe()

	go func() {
		defer func() {
			srvr.Close()
		}()

		tr := &transport{conn: srvr}

		for {
			select {
			case <-ctx.Done():
				return
			default:
				break
			}

			p, err := tr.recv()
			if err != nil {
				return
			}

			switch p.ptype {
			case pktEventRegister, pktEventUnregister:
				var ack *packet

				if p.name != "test-event" {
					ack = newPacket(pktEventUnknown, "", nil)
				} else {
					ack = newPacket(pktEventConfirm, "", nil)
				}

				err := tr.send(ack)
				if err != nil {
					return
				}

				if p.ptype == pktEventRegister {
					// Write one event message
					msg := NewMessage()
					err := msg.Set("test", "hello world!")
					if err != nil {
						log.Printf("Failed to set message field: %v", err)
					}
					event := newPacket(pktEvent, "test-event", msg)
					err = tr.send(event)
					if err != nil {
						log.Printf("Failed to send test-event message: %v", err)
					}
				}

			default:
				continue
			}
		}
	}()

	return client
}

func TestListenAndCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dctx, dcancel := context.WithCancel(context.Background())
	defer dcancel()

	conn := mockCharon(dctx)

	s, err := NewSession(withTestConn(conn))
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer s.Close()

	if err := s.Listen(ctx, "test-event"); err != nil {
		t.Fatalf("Failed to start event listener: %v", err)
	}

	e, err := s.NextEvent()
	if err != nil {
		t.Fatalf("Unexpected error on NextEvent: %v", err)
	}

	if e.Message.Get("test") != "hello world!" {
		t.Fatalf("Unexpected message: %v", e)
	}

	cancel()

	e, err = s.NextEvent()
	if err == nil {
		t.Fatalf("Expected error after closing listener, got message: %v", e)
	}
}

func TestListenAndCloseSession(t *testing.T) {
	dctx, dcancel := context.WithCancel(context.Background())
	defer dcancel()

	conn := mockCharon(dctx)

	s, err := NewSession(withTestConn(conn))
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer s.Close()

	err = s.Listen(context.Background(), "test-event")
	if err != nil {
		t.Fatalf("Failed to start event listener: %v", err)
	}

	e, err := s.NextEvent()
	if err != nil {
		t.Fatalf("Unexpected error on NextEvent: %v", err)
	}

	if e.Message.Get("test") != "hello world!" {
		t.Fatalf("Unexpected message: %v", e)
	}

	// Close session
	s.Close()

	e, err = s.NextEvent()
	if err == nil {
		t.Fatalf("Expected error after closing listener, got message: %v", e)
	}
}

func TestSessionClose(t *testing.T) {
	// Create a session without connecting to charon
	conn, _ := net.Pipe()

	s, err := NewSession(withTestConn(conn))
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Unpexected error when closing Session: %v", err)
	}
}

func TestIdempotentSessionClose(t *testing.T) {
	conn, _ := net.Pipe()

	s, err := NewSession(withTestConn(conn))
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Unpexected error when closing Session (first close): %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Unpexected error when closing Session (second close): %v", err)
	}
}

// These tests are considered 'integration' tests because they require charon
// to be running, and make actual client-issued commands. Note that these are
// only meant to test the package API, and the specific commands used are out
// of convenience; any command that satisfies the need of the test could be used.
//
// For example, TestStreamedCommandRequest uses the 'list-authorities' command, but
// any event-streaming vici command could be used.
//
// These tests are only run when the -integration flag is set to true.
var (
	doIntegrationTests = flag.Bool("integration", false, "Run integration tests that require charon")
)

func maybeSkipIntegrationTest(t *testing.T) {
	if !*doIntegrationTests {
		t.Skip("Skipping integration test.")
	}
}

// TestCommandRequest tests CommandRequest by calling the 'version' command.
// For good measure, check the response and make sure the 'daemon' field is
// set to 'charon.'
func TestCommandRequest(t *testing.T) {
	maybeSkipIntegrationTest(t)

	s, err := NewSession()
	if err != nil {
		t.Fatalf("Failed to create a session: %v", err)
	}
	defer s.Close()

	resp, err := s.CommandRequest("version", nil)
	if err != nil {
		t.Fatalf("Failed to get charon version information: %v", err)
	}

	if d := resp.Get("daemon"); d != "charon" {
		t.Fatalf("Got unexpected value for 'daemon' (%s)", d)
	}
}

// TestStreamedCommandRequest tests StreamedCommandRequest by calling the
// 'list-authorities' command. Likely, there will be no authorities returned,
// but make sure any Messages that are streamed have non-nil err.
func TestStreamedCommandRequest(t *testing.T) {
	maybeSkipIntegrationTest(t)

	s, err := NewSession()
	if err != nil {
		t.Fatalf("Failed to create a session: %v", err)
	}
	defer s.Close()

	ms, err := s.StreamedCommandRequest("list-authorities", "list-authority", nil)
	if err != nil {
		t.Fatalf("Failed to list authorities: %v", err)
	}

	for i, m := range ms.Messages() {
		if m.Err() != nil {
			t.Fatalf("Got error in message #%d: %v", i+1, m.Err())
		}
	}
}

func TestListenCancelListenAgain(t *testing.T) {
	maybeSkipIntegrationTest(t)

	s, err := NewSession()
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())

	err = s.Listen(ctx, "control-log")
	if err != nil {
		t.Fatalf("Failed to start event listener (first time): %v", err)
	}

	cancel()

	err = s.Listen(context.Background(), "control-log")
	if err != nil {
		t.Fatalf("Failed to start event listener (second time): %v", err)
	}
}

func TestListenWhenAlreadyActive(t *testing.T) {
	maybeSkipIntegrationTest(t)

	s, err := NewSession()
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer s.Close()

	if err := s.Listen(context.Background(), "control-log"); err != nil {
		t.Fatalf("Failed to start event listener: %v", err)
	}

	// This should return an error since an event listener was just registered.
	if err := s.Listen(context.Background(), "control-log"); err == nil {
		t.Fatal("Expected error when registering a second event listener!")
	}
}

func TestCloseWithActiveNextEvent(t *testing.T) {
	maybeSkipIntegrationTest(t)

	s, err := NewSession()
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	if err := s.Listen(context.Background(), "ike-updown"); err != nil {
		t.Fatalf("Failed to start event listener: %v", err)
	}

	done := make(chan struct{}, 1)

	go func() {
		defer close(done)

		_, err := s.NextEvent()
		if err == nil {
			t.Errorf("Expected error when reading event from closed listener")
		}
	}()

	// Sleep before closing to ensure that NextEvent is called
	// before Close.
	<-time.After(3 * time.Second)

	if err := s.Close(); err != nil {
		t.Fatalf("Unexpected error closing session: %v", err)
	}

	<-done
}

// TestEventNameIsSet tests NextEvent by making sure the event
// type name is properly set in the returned Event. This is done
// by listening for -- and triggering -- a 'log' event. The event
// is triggered by a call to 'reload-settings'.
func TestEventNameIsSet(t *testing.T) {
	maybeSkipIntegrationTest(t)

	s, err := NewSession()
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}
	defer s.Close()

	// Just in case the call to reload-settings doesn't trigger
	// an event, close the listener after 5 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Listen(ctx, "log"); err != nil {
		t.Fatalf("Failed to start event listener: %v", err)
	}

	// The event triggered by this command will be buffered in the event queue.
	if _, err := s.CommandRequest("reload-settings", nil); err != nil {
		t.Fatalf("Failed to send 'reload-settings' command: %v", err)
	}

	e, err := s.NextEvent()
	if err != nil {
		t.Fatalf("Unexpected error waiting for event: %v", err)
	}

	if e.Name != "log" {
		t.Fatalf("Expected to receive 'log' event, got %s", e.Name)
	}
}
