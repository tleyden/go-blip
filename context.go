package blip

import (
	"fmt"
	"log"
	"net/http"
	"runtime/debug"

	"code.google.com/p/go.net/websocket"
)

// A function that handles an incoming BLIP request and optionally sends a response.
// A handler is called on a new goroutine so it can take as long as it needs to.
// For example, if it has to send a synchronous network request before it can construct
// a response, that's fine.
type Handler func(request *Message)

func Unhandled(request *Message) {
	request.Response().SetError(BLIPErrorDomain, 404)
}

// Defines how incoming requests are dispatched to handler functions.
type Context struct {
	HandlerForProfile map[string]Handler // Handler function for a request Profile
	DefaultHandler    Handler            // Handler for all otherwise unhandled requests
	MaxSendQueueCount int                // Max # of messages being sent at once (if >0)
	LogMessages       bool               // If true, will log about messages
	LogFrames         bool               // If true, will log about frames (very verbose)
}

// Creates a new Context with an empty dispatch table.
func NewContext() *Context {
	return &Context{
		HandlerForProfile: map[string]Handler{},
	}
}

func (context *Context) start(ws *websocket.Conn) *Sender {
	r := newReceiver(context, ws)
	r.sender = newSender(context, ws, r)
	r.sender.start()
	return r.sender
}

// Opens a connection to a host.
func (context *Context) Dial(url string, origin string) (*Sender, error) {
	ws, err := websocket.Dial(url, "BLIP", origin)
	if err != nil {
		return nil, err
	}
	sender := context.start(ws)
	go sender.receiver.receiveLoop()
	return sender, nil
}

// Creates an HTTP handler that will receive connections for this Context
func (context *Context) HTTPHandler() http.Handler {
	return websocket.Handler(func(ws *websocket.Conn) {
		log.Printf("** Start handler...")
		sender := context.start(ws)
		err := sender.receiver.receiveLoop()
		if err != nil {
			log.Printf("** Handler exited: %v", err)
		}
		sender.Stop()
	})
}

func (context *Context) dispatchRequest(request *Message, sender *Sender) {
	defer func() {
		// On return/panic, send the response:
		response := request.Response()
		if panicked := recover(); panicked != nil {
			stack := debug.Stack()
			log.Printf("*** PANIC handling BLIP request %v: %v:\n%s", request, panicked, stack)
			if response != nil {
				response.SetError(BLIPErrorDomain, 500)
				response.Properties["Error-Message"] = fmt.Sprintf("Panic: %v", panicked)
			}
		}
		if response != nil {
			sender.send(response)
		}
	}()

	context.logMessage("INCOMING REQUEST: %s", request)
	handler := context.HandlerForProfile[request.Properties["Profile"]]
	if handler == nil {
		handler = context.DefaultHandler
		if handler == nil {
			handler = Unhandled
		}
	}
	handler(request)
}

func (context *Context) dispatchResponse(response *Message) {
	defer func() {
		// On return/panic, log a warning:
		if panicked := recover(); panicked != nil {
			stack := debug.Stack()
			log.Printf("*** PANIC handling BLIP response %v: %v:\n%s", response, panicked, stack)
		}
	}()

	context.logMessage("INCOMING RESPONSE: %s", response)
	//panic("UNIMPLEMENTED") //TODO
}

func (context *Context) log(fmt string, params ...interface{}) {
	log.Printf("BLIP: "+fmt, params...)
}

func (context *Context) logMessage(fmt string, params ...interface{}) {
	if context.LogMessages {
		context.log(fmt, params...)
	}
}

func (context *Context) logFrame(fmt string, params ...interface{}) {
	if context.LogFrames {
		context.log(fmt, params...)
	}
}

//  Copyright (c) 2013 Jens Alfke.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.
