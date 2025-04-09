package earlydata

import (
	"github.com/gofiber/fiber/v2"

	"github.com/oarkflow/router"
)

const (
	localsKeyAllowed = "earlydata_allowed"
)

func IsEarly(c *fiber.Ctx) bool {
	return c.Locals(localsKeyAllowed) != nil
}

// New creates a new middleware handler
// https://datatracker.ietf.org/doc/html/rfc8470#section-5.1
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Return new handler
	return func(c *fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return router.Next(c)
		}

		// Abort if we can't trust the early-data header
		if !c.IsProxyTrusted() {
			return cfg.Error
		}

		// Continue stack if request is not an early-data request
		if !cfg.IsEarlyData(c) {
			return router.Next(c)
		}

		// Continue stack if we allow early-data for this request
		if cfg.AllowEarlyData(c) {
			_ = c.Locals(localsKeyAllowed, true)
			return router.Next(c)
		}

		// Else return our error
		return cfg.Error
	}
}
