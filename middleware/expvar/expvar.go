package expvar

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/valyala/fasthttp/expvarhandler"

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

		path := c.Path()
		// We are only interested in /debug/vars routes
		if len(path) < 11 || !strings.HasPrefix(path, "/debug/vars") {
			return router.Next(c)
		}
		if path == "/debug/vars" {
			expvarhandler.ExpvarHandler(c.Context())
			return nil
		}

		return c.Redirect("/debug/vars", fiber.StatusFound)
	}
}
