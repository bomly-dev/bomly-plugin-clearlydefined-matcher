// Command bomly-plugin-clearlydefined-matcher serves the ClearlyDefined
// license matcher as a managed Bomly plugin over the HashiCorp go-plugin
// gRPC transport. The binary is launched and supervised by Bomly; it is not
// meant to be run by hand.
package main

import (
	sdk "github.com/bomly-dev/bomly-sdk"

	"github.com/bomly-dev/bomly-plugin-clearlydefined-matcher/plugin"
)

func main() { sdk.ServeModule(plugin.Module()) }
