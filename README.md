# BLIP-Over-WebSocket Implementation for Go

This is a [Go][GO] language [golang] implementation of the [BLIP][BLIP] messaging protocol running over [WebSockets][WEBSOCKET].

## Why?

BLIP adds several useful features that aren't supported directly by WebSocket:

* Request/response: Messages can have responses, and the responses don't have to be sent in the same order as the original messages. Responses are optional; a message can be sent in no-reply mode if it doesn't need one, otherwise a response (even an empty one) will always be sent after the message is handled.
* Metadata: Messages are structured, with a set of key/value headers and a binary body, much like HTTP or MIME messages. Peers can use the metadata to route incoming messages to different handlers, effectively creating multiple independent channels on the same connection.
* Multiplexing: Large messages are broken into fragments, and if multiple messages are ready to send their fragments will be interleaved on the connection, so they're sent in parallel. This prevents huge messages from blocking the connection.
* Priorities: Messages can be marked Urgent, which gives them higher priority in the multiplexing (but without completely starving normal-priority messages.) This is very useful for streaming media.

## Status

The code has been tested and is working well in a branch of the Couchbase Sync Gateway, but it's not being used in the master branch yet. --Jens 8/2015

## Protocol

Here's the [protocol documentation][BLIP_PROTOCOL].

## HTTP Over BLIP (Over WebSocket)

This repo includes some experimental support for layering HTTP over BLIP — that is, wrapping an HTTP request in a BLIP request, and its response in the BLIP response. The protocol is very simple: the body of the request and the response is simply the regular HTTP wire protocol. The BLIP properties are unused, although I've been setting the Profile property to "HTTP" to make the messages easy to recognize.

It may seem weird to layer HTTP over a protocol that's tunneling through HTTP in the first place, but this allows for an arbitrary number of simultaneous HTTP requests without having to worry about exceeding a limited socket pool. It also allows for server-initiated requests, which are good for "push" APIs.

(This may likely be obsoleted by HTTP 2, which has basically the same benefits.)

## Go API

### Server (Listening) Side

First create a BLIP Context and register one or more handler functions with it:

	context := blip.NewContext()
	context.HandlerForProfile["BLIPTest/EchoData"] = dispatchEcho

Then get the context's WebSocket HTTP handler and register it with Go's HTTP framework:

	http.Handle("/test", context.HTTPHandler())
	err := http.ListenAndServe(":12345", nil)

The BLIP handler function must take a `*blip.Message` as a parameter and return void. It should use the request's properties [headers] and body. If a response is appropriate and desired, the request's Response property will be another message, whose properties and body can be written to. When the handler returns the response will be sent.

	func dispatchEcho(request *blip.Message) {
		body, err := request.Body()
		if err != nil {
			log.Printf("ERROR reading body of %s: %s", request, err)
			return
		}
		for i, b := range body {
			if b != byte(i%256) {
				panic(fmt.Sprintf("Incorrect body: %x", body))
			}
		}
		if request.Properties["Content-Type"] != "application/octet-stream" {
			panic(fmt.Sprintf("Incorrect properties: %#x", request.Properties))
		}
		if response := request.Response(); response != nil {
			response.SetBody(body)
			response.Properties["Content-Type"] = request.Properties["Content-Type"]
		}
	}

The terms "Server" and "Client" are used loosely in BLIP. As in WebSockets, either peer in the connection can both send and receive messages.

### Client Side

First create a BLIP context and open a connection to the desired host:

	context := blip.NewContext()
	sender, err := context.Dial("ws://localhost:12345/test", "http://localhost")

The `sender` object is used to send messages:

		request := blip.NewRequest()
		request.SetProfile("BLIPTest/EchoData")
		request.SetBody(bodyData)
		sender.Send(request)

Sending the request is asynchronous -- the `Send` method returns immediately. Once the request has been sent, its response message can be obtained, although accessing the properties or body will block until the actual reply arrives. (In other words, the response is a type of _future_.)

	response := request.Response()
	body, err := response.Body()

As mentioned above, note that both 'server' and 'client' can initiate messages. If the client wants to receive messages (other than replies) from the server, it should register handlers with its context as shown in the previous section.

[GO]: http://golang.org
[BLIP]: https://bitbucket.org/snej/mynetwork/wiki/BLIP/Overview
[BLIP_PROTOCOL]: https://github.com/couchbaselabs/BLIP-Cocoa/blob/master/Docs/BLIP%20Protocol.md
[WEBSOCKET]: http://www.websocket.org
