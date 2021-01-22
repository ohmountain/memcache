package memcache

import (
	"io"
	"io/ioutil"
)

func (m *memcache) SetReader(reader io.Reader) {
	m.reader = reader
}

func (m *memcache) Read() error {
	_, err := ioutil.ReadAll(m.reader)
	if err != nil {
		return err
	}

	return nil
}
