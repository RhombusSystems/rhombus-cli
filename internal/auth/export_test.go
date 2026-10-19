package auth

import (
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

// SetTokenURL points refreshes at a test server and returns a restore func.
func SetTokenURL(u string) (restore func()) {
	orig := tokenURL
	tokenURL = func(config.Config) string { return u }
	return func() { tokenURL = orig }
}

// SetNow replaces the clock and returns a restore func.
func SetNow(f func() time.Time) (restore func()) {
	orig := now
	now = f
	return func() { now = orig }
}
