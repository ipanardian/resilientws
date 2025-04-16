## Resilient WebSocket Client for Go
[![Status](https://img.shields.io/badge/status-beta-green.svg)](https://github.com/ipanardian/resilientws/releases)
[![Go](https://img.shields.io/badge/go-v1.22.x-blue.svg)](https://gitter.im/ipanardian/resilientws)
[![GitHub license](https://img.shields.io/badge/license-MIT-red.svg)](https://github.com/ipanardian/GoBranch/blob/master/LICENSE)

`resilientws` is a lightweight and resilient WebSocket client library built in Go, designed for real-time data streaming applications that require automatic reconnection, subscription resumption, and heartbeat management.

## Feature
- *Connection Management* — Establish and maintain a resilient WebSocket connection using the provided configuration (Headers, TLSConfig, Proxy, Logger, PingInterval, PongTimeout, MessageQueueSize).
- *Automatic Reconnection* — Automatically reconnect to the WebSocket server if the connection is lost, with configurable retry intervals.
- *Subscribe Handler* — Invoke the SubscribeHandler function after each successful connection or reconnection event.
- *Message Handler* — Handle incoming messages from the WebSocket server.
- *Ping/Pong Mechanism* — Send ping messages at the configured PingInterval to keep the connection alive, using the PingHandler function.
- *Event Handling* — Provide callbacks for connection, reconnection, and error events (onConnected, onReconnecting, onError).
- *Message Queue* — Maintain a queue of messages to be sent when the connection is reestablished after a disconnection.

## Installation

```sh
go get github.com/ipanardian/resilientws
```

## Usage

Check the example directory for usage examples.

## License

[resilientws](https://github.com/ipanardian/resilientws) is licensed under the [MIT License](https://opensource.org/licenses/MIT).


`resilientws` inspired by [recws](https://github.com/recws-org/recws)
