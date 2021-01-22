package memcache

import (
	"encoding/gob"
	"os"
	"testing"
)

type User struct {
	Name     string
	Age      uint8
	internal uint8
}

func Test_Serialize(t *testing.T) {
	user := User{
		Name: "张三的歌",
	}

	file, _ := os.Create("cache.bin")

	encoder := gob.NewEncoder(file)

	err := encoder.Encode(user)
	t.Logf("Err: %v", err)

	file.Sync()

	file, _ = os.Open("cache.bin")
	decoder := gob.NewDecoder(file)

	next := User{}
	err = decoder.Decode(&next)
	t.Logf("Err1: %v", err)
	t.Logf("User1: %v", next)

}
