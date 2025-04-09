package requestid

import (
	"github.com/gofiber/fiber/v2"

	"github.com/oarkflow/router"
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Return new handler
	return func(c *fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return router.Next(c)
		}
		// Get id from request, else we generate one
		rid := c.Get(cfg.Header)
		if rid == "" {
			rid = cfg.Generator()
		}

		// Set new id to response header
		c.Set(cfg.Header, rid)

		// Add the request ID to locals
		c.Locals(cfg.ContextKey, rid)

		// Continue stack
		return router.Next(c)
	}
}
