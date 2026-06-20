// Package web embeds the built front-end (web/dist) so `oss-agent serve`/`ui`
// ships the whole app as a single binary. Run `npm --prefix web run build`
// before `go build` to (re)generate dist.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
