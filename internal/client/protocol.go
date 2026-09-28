package client

import "time"

// The messages of the GOCACHEPROG protocol, as the go command defines them in
// cmd/go/internal/cacheprog. The go command writes one Request per line on
// the client's stdin. For a put with BodySize > 0, the body follows on the
// next line as a base64-encoded JSON string. The client answers each Request
// with one Response on stdout, in any order. Its first message, with ID 0,
// lists the commands it knows.

type cmd string

const (
	cmdGet   cmd = "get"
	cmdPut   cmd = "put"
	cmdClose cmd = "close"
)

type request struct {
	ID       int64
	Command  cmd
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []cmd      `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}
