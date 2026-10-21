// Package web holds the dashboard: one static page that polls the local
// status API. It is embedded into the daemon so there is nothing to deploy.
package web

import "embed"

//go:embed index.html
var FS embed.FS
