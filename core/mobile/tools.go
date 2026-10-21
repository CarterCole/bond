//go:build tools

package mobile

// gomobile generates code that imports this package, so the module has to
// require it even though nothing here uses it directly. Importing it under a
// build tag keeps `go mod tidy` from dropping the requirement.
import _ "golang.org/x/mobile/bind"
