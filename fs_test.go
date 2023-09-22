package acp

import "testing"

func TestFS(t *testing.T) {
	mpCache, err := getMountpointCache()
	if err != nil {
		panic(err)
	}

	t.Log("mp cahce", mpCache("/Users/cuijingning/go/src/github.com/samuelncui/acp"))
}
