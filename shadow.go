package memcache

import (
	"bytes"
	"compress/zlib"
	"encoding/gob"
	"fmt"
	"os"
)

type shadow struct {
	// 版本号
	Version int64

	// 当前已缓存数量
	Size uint

	// 允许缓存容量
	Cap   uint
	Nodes []node
}

func (s *shadow) memcache() memcache {
	m := memcache{
		size:   s.Size,
		cap:    s.Cap,
		locker: make(chan struct{}, 1),
		holder: make(map[string]*node),
	}
	if m.size == 0 {
		return m
	}

	for i := 0; i < int(m.size); i++ {
		cur := s.Nodes[i]
		m.holder[cur.Key] = &cur
	}

	for i := 0; i < int(m.size); i++ {
		n := m.holder[s.Nodes[i].Key]

		if i == 0 {
			m.header = n
		}

		if i == int(m.size)-1 {
			m.tail = n
		}

		if i < int(m.size-1) {
			nxt := m.holder[s.Nodes[i+1].Key]
			n.Nxt = nxt
			nxt.Prv = n
		}

	}

	return m
}

func (s *shadow) Persist() error {
	file, err := os.Create(fmt.Sprintf("%d.bin", s.Version))
	if err != nil {
		return err
	}
	defer func() {
		file.Sync()
		file.Close()
	}()

	var b = bytes.Buffer{}
	w := zlib.NewWriter(&b)

	defer func() {
		file.Write(b.Bytes())
		w.Close()
	}()

	return gob.NewEncoder(w).Encode(s)
}

func (s *shadow) FromFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	r, err := zlib.NewReader(file)
	if err != nil {
		return err
	}
	defer r.Close()

	return gob.NewDecoder(r).Decode(&s)
}
