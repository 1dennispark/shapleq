//go:build !release

package constants

import "os"

var (
	DefaultHomeDir = os.ExpandEnv("$HOME/.pirius-debug")
)
