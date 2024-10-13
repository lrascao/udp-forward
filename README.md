# udp-forward

A dead simple Go (golang) package to forward UDP packets like a reverse NAT (i.e. it supports multiple users).

## Usage

```go
package main

import (
    "github.com/lrascao/udp-forward"
    "log/slog"
)

func main() {
	forwarder, err := forward.NewForwarder("0.0.0.0:1000",
		forward.WithDestination("target1", "1.2.3.4:1023"),
		forward.WithTimeout(30*time.Second),
		forward.WithConnectCallback(func(addr string) {
			slog.Debug("connected", "from", addr)
		}),
		forward.WithDisconnectCallback(func(addr string) {
			slog.Debug("disconnected", "from", addr)
		}),
	)
	// Do something...

	// Stop the forwarder
	forwarder.Close()
}
```

See the [GoDoc](https://godoc.org/github.com/1lann/udp-forward) for documentation.

## License

There is [no license](/LICENSE).
