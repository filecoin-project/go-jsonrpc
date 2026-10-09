package jsonrpc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type requestWaitContext struct {
	context.Context
	resume <-chan struct{}
}

func (c requestWaitContext) Done() <-chan struct{} {
	<-c.resume
	return c.Context.Done()
}

func TestClientPendingRequestUnblocksOnExit(t *testing.T) {
	for _, queuedReply := range []bool{false, true} {
		name := "no reply"
		if queuedReply {
			name = "queued reply"
		}
		t.Run(name, func(t *testing.T) {
			attempts := 1
			if queuedReply {
				// Both receive cases are ready on every attempt. Select chooses
				// between them randomly, so exercise the synchronized race repeatedly.
				attempts = 64
			}
			for range attempts {
				checkPendingRequestExit(t, queuedReply)
			}
		})
	}
}

func checkPendingRequestExit(t *testing.T, queuedReply bool) {
	t.Helper()
	exiting := make(chan struct{})
	stop := sync.OnceFunc(func() { close(exiting) })
	defer stop()
	resume := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	defer unblock()
	c := &client{exiting: exiting}
	requests := c.setupRequestChan()
	cr := clientRequest{
		req:   request{Jsonrpc: "2.0", ID: float64(1)},
		ready: make(chan clientResponse, 1),
	}
	type outcome struct {
		response clientResponse
		err      error
	}
	result := make(chan outcome, 1)
	go func() {
		// Done is called after submission and before waiting for a response.
		ctx := requestWaitContext{Context: context.Background(), resume: resume}
		response, err := c.doRequest(ctx, cr)
		result <- outcome{response, err}
	}()

	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("request was not submitted")
	}
	if queuedReply {
		cr.ready <- clientResponse{Jsonrpc: "2.0", ID: cr.req.ID}
	}
	stop()
	// The reply cannot be consumed until the connection has also exited.
	unblock()

	select {
	case got := <-result:
		if queuedReply {
			if got.err != nil || got.response.ID != cr.req.ID {
				t.Fatalf("queued response lost on exit: response=%+v error=%v", got.response, got.err)
			}
		} else if got.err == nil || !strings.Contains(got.err.Error(), "exiting") {
			t.Fatalf("pending request error = %v, want connection exit error", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending request did not unblock when connection exited")
	}
}

func TestClientChannelCompletionDrainsBufferedValues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	value, constructor := (&client{}).makeOutChan(ctx, reflect.TypeFor[func() <-chan int](), 0)
	lifetime, sink := constructor()
	output := value().Interface().(<-chan int)

	for _, encoded := range []string{"1", "2", "3"} {
		sink([]byte(encoded), true)
	}
	sink(nil, false)

	for want := 1; want <= 3; want++ {
		select {
		case got, ok := <-output:
			if !ok || got != want {
				t.Fatalf("channel value = %d, open=%v; want %d", got, ok, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("buffered channel values did not drain")
		}
	}
	select {
	case _, ok := <-output:
		if ok {
			t.Fatal("channel stayed open after all buffered values drained")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed output channel did not close")
	}
	select {
	case <-lifetime.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("completed channel context was retained")
	}
	if context.Cause(lifetime) != errChannelClosed || ctx.Err() != nil {
		t.Fatalf("normal completion cause=%v, parent error=%v", context.Cause(lifetime), ctx.Err())
	}
}

func TestClientChannelCancellationClosesOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	value, constructor := (&client{}).makeOutChan(ctx, reflect.TypeFor[func() <-chan int](), 0)
	lifetime, sink := constructor()
	output := value().Interface().(<-chan int)
	cancel()

	select {
	case _, ok := <-output:
		if ok {
			t.Fatal("canceled output channel stayed open")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceling the caller did not stop the channel worker")
	}
	if !errors.Is(context.Cause(lifetime), context.Canceled) {
		t.Fatalf("caller cancellation cause = %v, want context.Canceled", context.Cause(lifetime))
	}
	sink(nil, false)
}
