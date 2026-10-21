//go:build !fixture

package main

import (
	"flag"

	"github.com/ondryadev/ws-ping/probe/internal/app"
)

// fixtureSetup registers nothing in production builds, so test-only flags such
// as --test-delay are rejected as unknown.
func fixtureSetup(*flag.FlagSet) func(*app.Options) { return nil }
