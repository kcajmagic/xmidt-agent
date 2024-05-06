// SPDX-FileCopyrightText: 2024 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package qos

import (
	"context"
	"errors"
	"sync"

	"github.com/xmidt-org/wrp-go/v3"
	"github.com/xmidt-org/xmidt-agent/internal/wrpkit"
)

var (
	ErrInvalidInput     = errors.New("invalid input")
	ErrMisconfiguredQOS = errors.New("misconfigured QOS")
	ErrQOSHasShutdown   = errors.New("QOS has been shutdown")
)

// Option is a functional option type for QOS.
type Option interface {
	apply(*Handler) error
}

type optionFunc func(*Handler) error

func (f optionFunc) apply(c *Handler) error {
	return f(c)
}

type serviceQOSHandler func(wrp.Message) (<-chan wrp.Message, <-chan struct{})

// Handler queues incoming messages and sends them to the next wrphandler
type Handler struct {
	next serviceQOSHandler
	// queue for wrp messages, ingested by serviceQOS
	queue chan wrp.Message
	// maxQueueSize is the allowable max size of the qos' priority queue, based on the sum of all queued wrp message's payload
	maxQueueSize int
	// MaxMessageBytes is the largest allowable wrp message payload.
	maxMessageBytes int

	lock   sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a new instance of the Handler struct.  The parameter next is the
// handler that will be called and monitored for errors.
// Note, once cancel is called, any calls to Handler.HandleWrp will result in
// an ErrQOSHasShutdown error
func New(next wrpkit.Handler, opts ...Option) (h *Handler, err error) {
	if next == nil {
		return nil, ErrInvalidInput
	}

	opts = append(opts, validateQueueConstraints())

	h = &Handler{
		next: curryWRPHandler(next),
	}

	var errs error
	for _, opt := range opts {
		if opt != nil {
			if err := opt.apply(h); err != nil {
				errs = errors.Join(errs, err)
			}
		}
	}

	if errs != nil {
		h = nil
	}

	return h, errs
}

func (h *Handler) Start() error {
	h.lock.Lock()
	defer h.lock.Unlock()

	if h.ctx != nil {
		return nil
	}

	h.queue = make(chan wrp.Message)
	// h.cancel() stops serviceQOS by closing its `done` chan.
	h.ctx, h.cancel = context.WithCancel(context.Background())
	go serviceQOS(h.ctx.Done(), h.queue, h.maxQueueSize, h.maxMessageBytes, h.next)

	return nil
}

func (h *Handler) Stop() {
	h.lock.Lock()
	defer h.lock.Unlock()

	if h.ctx == nil {
		return
	}

	h.cancel()
	h.ctx = nil
	h.cancel = nil
	// defensive: cancelling the context should be enough, but this makes things bulletproof
	close(h.queue)
	h.queue = nil
}

// HandleWRP queues incoming messages while the background serviceQOS goroutine attempts
// to send as many queued messages as possible, where the highest QOS messages are prioritized
func (h *Handler) HandleWrp(msg wrp.Message) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	if h.queue == nil {
		return ErrQOSHasShutdown
	}

	h.queue <- msg

	return nil
}

func curryWRPHandler(next wrpkit.Handler) serviceQOSHandler {
	return func(msg wrp.Message) (<-chan wrp.Message, <-chan struct{}) {
		ready := make(chan struct{})
		failedMsg := make(chan wrp.Message, 1)
		go func() {
			defer close(ready)
			defer close(failedMsg)

			// Note, Websocket.HandleWrp already locks between writes.
			if err := next.HandleWrp(msg); err != nil {
				// Delivery failed, re-enqueue message and try again later.
				failedMsg <- msg
				// The err itself is ignored.
			}
		}()

		return failedMsg, ready
	}
}

// serviceQOS is a long running goroutine that sends as many queued messages as possible,
// where the highest QOS messages are prioritized.
// serviceQOS starts when Handler.Start() is called.
// serviceQOS stops when Handler.Stop() is called, closing its `done` chan.
func serviceQOS(done <-chan struct{}, queue <-chan wrp.Message, maxQueueSize, maxMessageBytes int, handleWRP serviceQOSHandler) {
	// create and manage the priority queue
	pq := priorityQueue{maxQueueSize: maxQueueSize, maxMessageBytes: maxMessageBytes}
	var (
		// Signaling channel from the handleWRP.
		ready <-chan struct{}
		// Channel for failed deliveries, re-enqueue message.
		failedMsg <-chan wrp.Message
	)

	for {
		select {
		case <-done:
			return
		case msg, ok := <-queue:
			if !ok {
				// Don't enqueue an empty wrp.Message{}
				// Handler.Stop() has been called, both `queue` and `done` are closed.
				return
			}

			pq.Enqueue(msg)
			if ready != nil {
				// Previous handleWRP call has not finished, do nothing.
			} else if top, ok := pq.Dequeue(); ok {
				failedMsg, ready = handleWRP(top)
			}
		case <-ready:
			// Previous handleWRP call has finished, check whether handleWRP
			// had successfully delivered its message or not.
			// If it failed, then failedMsg will contain the failed message.
			// Otherwise failedMsg is closed.
			if msg, ok := <-failedMsg; ok {
				// Delivery failed, re-enqueue message and try again later.
				pq.Enqueue(msg)
			}

			ready, failedMsg = nil, nil
			if top, ok := pq.Dequeue(); ok {
				failedMsg, ready = handleWRP(top)
			}
		}
	}
}
