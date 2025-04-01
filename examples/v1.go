package main

import (
	"log"

	"github.com/gofiber/fiber/v2"

	"github.com/oarkflow/router"
)

func main() {
	// Create a new Fiber app.
	app := fiber.New()

	// Create a new dynamic router based on our router package.
	dynamicRouter := router.New(app)

	// Set a custom NotFoundHandler.
	dynamicRouter.SetNotFoundHandler(func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Custom 404: Route not found",
		})
	})

	// Create an API group with a common prefix and a middleware.
	apiGroup := dynamicRouter.Group("/api", func(c *fiber.Ctx) error {
		log.Println("API group middleware executed")
		return router.Next(c)
	})

	// Define API routes under the /api prefix.
	apiGroup.Get("/hello", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"message": "Hello from /api/hello"})
	})

	apiGroup.Post("/data", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "data received"})
	})

	// Create a subgroup for versioned endpoints under /api.
	v1 := apiGroup.Group("/v1")
	v1.Get("/users", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"users": []string{"Alice", "Bob", "Charlie"}})
	})

	// Create an admin group with its own middleware (e.g., an auth check).
	adminGroup := dynamicRouter.Group("/admin", func(c *fiber.Ctx) error {
		// A simple token check for demonstration purposes.
		if c.Query("token") != "secret" {
			return c.Status(fiber.StatusUnauthorized).SendString("Unauthorized")
		}
		return router.Next(c)
	})
	adminGroup.Get("/dashboard", func(c *fiber.Ctx) error {
		return c.SendString("Welcome to the admin dashboard!")
	})

	// Register a static route group. The group's prefix will be prepended.
	// Files will be served from the "./public" directory.
	staticGroup := dynamicRouter.Group("/static")
	staticGroup.Static("", "./public", router.StaticConfig{
		Compress:     true,
		CacheControl: "public, max-age=86400",
	})

	// Log all registered routes for debugging.
	log.Println("Registered routes:", dynamicRouter.ListRoutes())

	// Start the server.
	log.Fatal(app.Listen(":3000"))
}
