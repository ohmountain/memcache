package memcache

import (
	"errors"
	"io"
)

func (m *memcache) SetWriter(writer io.Writer) {
	m.writer = writer
}

// TODO
func (m *memcache) Persist() (int, error) {

	if m.writer == nil {
		return 0, errors.New("m.writer is nil")
	}

	// TODO
	return m.writer.Write(nil)
}
