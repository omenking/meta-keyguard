package keyguard

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

// Chrome refuses host-to-extension messages larger than 1 MB.
const maxOutgoing = 1024 * 1024

// ReadMessage reads one native message: a 32-bit length in native byte order, then UTF-8 JSON.
// It returns io.EOF when Chrome has closed the pipe.
func ReadMessage(r io.Reader) (map[string]any, error) {
	var length uint32
	if err := binary.Read(r, binary.NativeEndian, &length); err != nil {
		return nil, io.EOF
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, io.EOF
	}
	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, err
	}
	return message, nil
}

func WriteMessage(w io.Writer, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(body) > maxOutgoing {
		return errors.New("native message exceeds the 1 MB limit")
	}
	header := make([]byte, 4)
	binary.NativeEndian.PutUint32(header, uint32(len(body)))
	_, err = w.Write(append(header, body...))
	return err
}
