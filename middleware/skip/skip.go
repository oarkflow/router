package skip

import (
	"github.com/gofiber/fiber/v2"

	"github.com/oarkflow/router"
)

// New creates a middleware handler which skips the wrapped handler
// if the exclude predicate returns true.
func New(handler fiber.Handler, exclude func(c *fiber.Ctx) bool) fiber.Handler {
	if exclude == nil {
		return handler
	}

	return func(c *fiber.Ctx) error {
		if exclude(c) {
			return router.Next(c)
		}

		return handler(c)
	}
}
