package healthcheck

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"

	"github.com/oarkflow/router"
)

// HealthChecker defines a function to check liveness or readiness of the application
type HealthChecker func(*fiber.Ctx) bool

// ProbeCheckerHandler defines a function that returns a ProbeChecker
type HealthCheckerHandler func(HealthChecker) fiber.Handler

func healthCheckerHandler(checker HealthChecker) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if checker == nil {
			return router.Next(c)
		}

		if checker(c) {
			return c.SendStatus(fiber.StatusOK)
		}

		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
}

func New(config ...Config) fiber.Handler {
	cfg := defaultConfig(config...)

	isLiveHandler := healthCheckerHandler(cfg.LivenessProbe)
	isReadyHandler := healthCheckerHandler(cfg.ReadinessProbe)

	return func(c *fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return router.Next(c)
		}

		if c.Method() != fiber.MethodGet {
			return router.Next(c)
		}

		prefixCount := len(utils.TrimRight(c.Route().Path, '/'))
		if len(c.Path()) >= prefixCount {
			checkPath := c.Path()[prefixCount:]
			checkPathTrimmed := checkPath
			if !c.App().Config().StrictRouting {
				checkPathTrimmed = utils.TrimRight(checkPath, '/')
			}
			switch {
			case checkPath == cfg.ReadinessEndpoint || checkPathTrimmed == cfg.ReadinessEndpoint:
				return isReadyHandler(c)
			case checkPath == cfg.LivenessEndpoint || checkPathTrimmed == cfg.LivenessEndpoint:
				return isLiveHandler(c)
			}
		}

		return router.Next(c)
	}
}
