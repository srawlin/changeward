// Command changegate-adapter-toykv serves the toy KV adapter as a plugin.
package main

import (
	"github.com/srawlin/changegate/adapters/toykv"
	"github.com/srawlin/changegate/pkg/adapter"
)

func main() { adapter.Serve(toykv.New()) }
