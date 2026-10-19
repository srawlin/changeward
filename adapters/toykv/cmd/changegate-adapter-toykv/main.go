// Command changegate-adapter-toykv serves the toy KV adapter as a plugin.
package main

import (
	"github.com/changegate/changegate/adapters/toykv"
	"github.com/changegate/changegate/pkg/adapter"
)

func main() { adapter.Serve(toykv.New()) }
