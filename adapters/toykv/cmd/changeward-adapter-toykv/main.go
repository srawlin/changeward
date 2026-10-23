// Command changeward-adapter-toykv serves the toy KV adapter as a plugin.
package main

import (
	"github.com/srawlin/changeward/adapters/toykv"
	"github.com/srawlin/changeward/pkg/adapter"
)

func main() { adapter.Serve(toykv.New()) }
