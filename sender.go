package blip

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"

	"golang.org/x/net/websocket"
)

// Size of frame to send by default. This is arbitrary.
const kDefaultFrameSize = 4096
const kBigFrameSize = 4 * kDefaultFrameSize

const kAckInterval = 50000      // How often to send ACKs
const kMaxUnackedBytes = 128000 // Pause message when this many bytes are sent unacked

type msgKey struct {
	msgNo   MessageNumber
	msgType MessageType
}

// The sending side of a BLIP connection. Used to send requests and to close the connection.
type Sender struct {
	context         *Context
	conn            *websocket.Conn
	receiver        *receiver
	queue           *messageQueue
	icebox          map[msgKey]*Message
	curMsg          *Message
	numRequestsSent MessageNumber
	requeueLock     sync.Mutex
}

func newSender(context *Context, conn *websocket.Conn, receiver *receiver) *Sender {
	return &Sender{
		context:  context,
		conn:     conn,
		receiver: receiver,
		queue:    newMessageQueue(context),
		icebox:   map[msgKey]*Message{},
	}
}

// The IP address of the remote peer.
func (s *Sender) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

// Sends a new outgoing request to be delivered asynchronously.
// Returns false if the message can't be queued because the Sender has stopped.
func (sender *Sender) Send(msg *Message) bool {
	if msg.Type() != RequestType {
		panic("Don't send responses using Sender.Send")
	} else if !msg.Outgoing {
		panic("Can't send an incoming message")
	}
	return sender.send(msg)
}

// Posts a request or response to be delivered asynchronously.
// Returns false if the message can't be queued because the Sender has stopped.
func (sender *Sender) send(msg *Message) bool {
	if msg.Sender != nil || msg.encoder != nil {
		panic("Message is already enqueued")
	}
	msg.Sender = sender

	if !sender.queue.push(msg) {
		return false
	}

	if msg.Type() == RequestType && !msg.NoReply() {
		response := msg.createResponse()
		writer := response.asyncRead(func(err error) {
			msg.responseComplete(response)
		})
		sender.receiver.awaitResponse(response, writer)
	}
	return true
}

// Returns statistics about the number of incoming and outgoing messages queued.
func (sender *Sender) Backlog() (incomingRequests, incomingResponses, outgoingRequests, outgoingResponses int) {
	incomingRequests, incomingResponses = sender.receiver.backlog()
	outgoingRequests, outgoingResponses = sender.queue.backlog()
	return
}

// Stops the sender's goroutine.
func (sender *Sender) Stop() {
	sender.queue.stop()
}

func (sender *Sender) Close() {
	sender.Stop()
	sender.conn.Close()
}

// Spawns a goroutine that will write frames to the connection until Stop() is called.
func (sender *Sender) start() {
	sender.conn.PayloadType = websocket.BinaryFrame
	go (func() {
		sender.context.log("Sender starting...")
		buffer := bytes.NewBuffer(make([]byte, 0, kBigFrameSize))
		for {
			msg := sender.popNextMessage()
			if msg == nil {
				break
			}
			// As an optimization, allow message to send a big frame unless there's a higher-priority
			// message right behind it:
			maxSize := kBigFrameSize
			if !msg.Urgent() && sender.queue.nextMessageIsUrgent() {
				maxSize = kDefaultFrameSize
			}

			body, flags := msg.nextFrameToSend(maxSize - 10)

			sender.context.logFrame("Sending frame: %v (flags=%8b, size=%5d)", msg, flags, len(body))
			var header [2 * binary.MaxVarintLen64]byte
			i := binary.PutUvarint(header[:], uint64(msg.number))
			i += binary.PutUvarint(header[i:], uint64(flags))
			buffer.Write(header[:i])
			buffer.Write(body)
			sender.conn.Write(buffer.Bytes())
			buffer.Reset()

			if (flags & kMoreComing) != 0 {
				if len(body) == 0 {
					panic("empty frame should not have moreComing")
				}
				sender.requeue(msg, uint64(len(body)))
			}
		}
		sender.context.log("Sender stopped")
	})()
}

//////// FLOW CONTROL:

func (sender *Sender) popNextMessage() *Message {
	sender.requeueLock.Lock()
	sender.curMsg = nil
	sender.requeueLock.Unlock()

	msg := sender.queue.first()
	if msg == nil {
		return nil
	}

	sender.requeueLock.Lock()
	defer sender.requeueLock.Unlock()
	sender.curMsg = msg
	sender.queue.pop()
	return msg
}

func (sender *Sender) requeue(msg *Message, bytesSent uint64) {
	sender.requeueLock.Lock()
	defer sender.requeueLock.Unlock()
	msg.bytesSent += bytesSent
	if msg.bytesSent <= msg.bytesAcked+kMaxUnackedBytes {
		// requeue it so it can send its next frame later
		sender.queue.push(msg)
	} else {
		// or pause it till it gets an ACK
		sender.context.logFrame("Pausing %v", msg)
		sender.icebox[msgKey{msgNo: msg.number, msgType: msg.Type()}] = msg
	}
}

func (sender *Sender) receivedAck(requestNumber MessageNumber, msgType MessageType, bytesReceived uint64) {
	sender.context.logFrame("Received ACK of %s#%d (%d bytes)", kMessageTypeName[msgType], requestNumber, bytesReceived)
	sender.requeueLock.Lock()
	defer sender.requeueLock.Unlock()
	if msg := sender.queue.find(requestNumber, msgType); msg != nil {
		msg.bytesAcked = bytesReceived
	} else if msg := sender.curMsg; msg != nil && msg.number == requestNumber && msg.Type() == msgType {
		msg.bytesAcked = bytesReceived
	} else {
		key := msgKey{msgNo: requestNumber, msgType: msgType}
		if msg := sender.icebox[key]; msg != nil {
			msg.bytesAcked = bytesReceived
			if msg.bytesSent <= msg.bytesAcked+kMaxUnackedBytes {
				sender.context.logFrame("Resuming %v", msg)
				delete(sender.icebox, key)
				sender.queue.push(msg)
			}
		}
	}
}

func (sender *Sender) sendAck(msgNo MessageNumber, msgType MessageType, bytesReceived uint64) {
	flags := kNoReply | kUrgent
	sender.context.logFrame("Sending ACK of %s#%d (%d bytes)", kMessageTypeName[msgType], msgNo, bytesReceived)
	if msgType == RequestType {
		flags |= frameFlags(AckRequestType)
	} else {
		flags |= frameFlags(AckResponseType)
	}
	var buffer [3 * binary.MaxVarintLen64]byte
	i := binary.PutUvarint(buffer[:], uint64(msgNo))
	i += binary.PutUvarint(buffer[i:], uint64(flags))
	i += binary.PutUvarint(buffer[i:], uint64(bytesReceived))
	sender.conn.Write(buffer[0:i])

}

//  Copyright (c) 2013 Jens Alfke.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.
