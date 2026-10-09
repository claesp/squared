package catalog_test

import (
	"testing/fstest"
)

func mapFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, data := range files {
		m[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return m
}
