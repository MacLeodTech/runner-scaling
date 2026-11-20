// Package examples embeds the example deployment files so `runner-scaling init` can write them.
package examples

import "embed"

//go:embed compose.yaml guard-pools.yaml env.example
var FS embed.FS
