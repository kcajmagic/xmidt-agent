// SPDX-FileCopyrightText: 2024 Comcast Cable Communications Management, LLC
// SPDX-License-Identifier: Apache-2.0

package qos

import (
	"container/heap"
	"errors"
	"fmt"
	"time"

	"github.com/xmidt-org/wrp-go/v3"
)

var ErrMaxMessageBytes = errors.New("wrp message payload exceeds maxMessageBytes")

// priorityQueue implements heap.Interface and holds wrp Message, using wrp.QOSValue as its priority.
// https://xmidt.io/docs/wrp/basics/#qos-description-qos
type priorityQueue struct {
	// queue for wrp messages, ingested by serviceQOS
	queue []item
	// prioritizeOldest determines whether to prioritize the oldest message during a QualityOfService tie breaker,
	// with the default being to prioritize the newest messages.
	prioritizeOldest bool
	// prioritizeLowestQOS determines whether to prioritize messages with the lowest QualityOfService,
	// with the default being to prioritize the highest QualityOfService messages.
	// Only used during queue trimming.
	prioritizeLowestQOS bool
	// maxQueueBytes is the allowable max size of the queue based on the sum of all queued wrp message's payloads
	maxQueueBytes int64
	// MaxMessageBytes is the largest allowable wrp message payload.
	maxMessageBytes int
	// sizeBytes is the sum of all queued wrp message's payloads.
	// An int64 overflow is unlikely since that'll be over 9*10^18 bytes
	sizeBytes int64
}

type item struct {
	msg       wrp.Message
	timestamp time.Time
}

// Dequeue returns the next highest priority message.
func (pq *priorityQueue) Dequeue() (wrp.Message, bool) {
	// Required, otherwise heap.Pop will panic during an internal Swap call.
	if pq.Len() == 0 {
		return wrp.Message{}, false
	}

	msg, ok := heap.Pop(pq).(wrp.Message)

	return msg, ok
}

// Enqueue queues the given message.
func (pq *priorityQueue) Enqueue(msg wrp.Message) error {
	// Check whether msg violates maxMessageBytes.
	if len(msg.Payload) > pq.maxMessageBytes {
		return fmt.Errorf("%w: %v", ErrMaxMessageBytes, pq.maxMessageBytes)
	}

	heap.Push(pq, msg)
	pq.trim()
	return nil
}

// trim removes messages with the lowest QualityOfService (taking `prioritizeOldest` into account)
// until the queue no longer violates `maxQueueSize“.
func (pq *priorityQueue) trim() {
	if pq.sizeBytes <= pq.maxQueueBytes {
		return
	}

	// Prioritize messages with the lowest QualityOfService such that `pq.Pop()` will return said messages.
	pq.prioritizeLowestQOS = true
	defer func() {
		// Re-prioritize messages with the highest QualityOfService.
		pq.prioritizeLowestQOS = false
		// heap.Init() is required since the prioritization was switch to messages with the highest QualityOfService.
		// The complexity of heap.Init() is O(n) where n = h.Len().
		heap.Init(pq)
	}()

	// heap.Init() is required since the prioritization was switch to messages with the lowest QualityOfService.
	// The complexity of heap.Init() is O(n) where n = h.Len().
	heap.Init(pq)
	// trim until the queue no longer violates maxQueueBytes.
	for pq.sizeBytes > pq.maxQueueBytes {
		// Dequeue messages with the lowest QualityOfService.
		pq.Dequeue()
	}
}

// heap.Interface related implementations https://pkg.go.dev/container/heap#Interface

func (pq *priorityQueue) Len() int { return len(pq.queue) }

func (pq *priorityQueue) Less(i, j int) bool {
	iQOS := pq.queue[i].msg.QualityOfService
	jQOS := pq.queue[j].msg.QualityOfService
	iTimestamp := pq.queue[i].timestamp
	jTimestamp := pq.queue[j].timestamp

	// Determine whether a tie breaker is required.
	if iQOS == jQOS {
		if pq.prioritizeOldest {
			// Prioritize the oldest messages.
			return iTimestamp.Before(jTimestamp)
		}

		// Prioritize the newest messages.
		return iTimestamp.After(jTimestamp)
	} else if pq.prioritizeLowestQOS {
		// Prioritize messages with the lowest QualityOfService.
		return iQOS < jQOS
	}

	// Prioritize messages with the highest QualityOfService.
	return iQOS > jQOS

}

func (pq *priorityQueue) Swap(i, j int) {
	pq.queue[i], pq.queue[j] = pq.queue[j], pq.queue[i]
}

func (pq *priorityQueue) Push(x any) {
	item := item{msg: x.(wrp.Message), timestamp: time.Now()}
	pq.sizeBytes += int64(len(item.msg.Payload))
	pq.queue = append(pq.queue, item)
}

func (pq *priorityQueue) Pop() any {
	last := len(pq.queue) - 1
	if last < 0 {
		return nil
	}

	msg := pq.queue[last].msg
	pq.sizeBytes -= int64(len(msg.Payload))
	// avoid memory leak
	pq.queue[last] = item{}
	pq.queue = pq.queue[0:last]

	return msg
}
