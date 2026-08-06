package engineerwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// maxLine bounds a single NDJSON message, mirroring internal/mcp/stdio.go.
const maxLine = 1 << 20

// Marshal encodes one message as a single NDJSON line, trailing newline
// included, and stamps its type field (and, for Hello, ProtocolVersion)
// itself — a caller only ever fills in the payload. It rejects anything that
// is not one of the seven wire message types.
func Marshal(msg any) ([]byte, error) {
	var v any
	switch m := msg.(type) {
	case Spec:
		m.Type = TypeSpec
		v = m
	case Answer:
		m.Type = TypeAnswer
		v = m
	case Cancel:
		m.Type = TypeCancel
		v = m
	case Hello:
		m.Type = TypeHello
		m.ProtocolVersion = ProtocolVersion
		v = m
	case Event:
		m.Type = TypeEvent
		v = m
	case Question:
		m.Type = TypeQuestion
		v = m
	case Result:
		m.Type = TypeResult
		v = m
	default:
		return nil, fmt.Errorf("engineerwire: cannot marshal unknown message type %T", msg)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Decoder reads one NDJSON message at a time, decoding the envelope's type
// field before unmarshalling into the concrete struct it names.
type Decoder struct {
	br *bufio.Reader
}

// NewDecoder wraps r. Line length is bounded the same way
// internal/mcp/stdio.go bounds a JSON-RPC message.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{br: bufio.NewReaderSize(r, maxLine)}
}

// Decode reads and returns the next message as one of the seven concrete
// wire types (boxed in the any return). It returns io.EOF once the stream is
// exhausted.
func (d *Decoder) Decode() (any, error) {
	for {
		line, err := d.br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		return decodeLine(trimmed)
	}
}

// decodeLine decodes one complete line: the envelope's type field first,
// then the concrete struct it names. Shared by Decoder and Journal, which
// both ultimately decode the same wire lines — one live, one from disk.
func decodeLine(line []byte) (any, error) {
	var env struct {
		Type Type `json:"type"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("engineerwire: decode envelope: %w", err)
	}
	switch env.Type {
	case TypeSpec:
		var m Spec
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return m, nil
	case TypeAnswer:
		var m Answer
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return m, nil
	case TypeCancel:
		var m Cancel
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return m, nil
	case TypeHello:
		var m Hello
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return m, nil
	case TypeEvent:
		var m Event
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return m, nil
	case TypeQuestion:
		var m Question
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return m, nil
	case TypeResult:
		var m Result
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return m, nil
	default:
		return nil, fmt.Errorf("engineerwire: unknown message type %q", env.Type)
	}
}
