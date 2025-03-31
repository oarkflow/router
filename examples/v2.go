package main

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"

	"github.com/oarkflow/router"
)

func main() {
	// Create a new Fiber app with an HTML engine.
	engine := html.New("./static/dist", ".html")
	app := fiber.New(fiber.Config{
		Views: engine,
	})

	// Create a dynamic router using our custom router package.
	dynRouter := router.New(app)

	// Set a custom NotFoundHandler.
	dynRouter.SetNotFoundHandler(func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Custom 404: Route not found",
		})
	})

	// ----------------------
	// Normal Routes
	// ----------------------
	// Add a normal route.
	dynRouter.AddRoute("GET", "/hello", func(c *fiber.Ctx) error {
		return c.SendString("Hello, world!")
	})

	// After 10 seconds, update the handler for /hello.
	go func() {
		time.Sleep(10 * time.Second)
		dynRouter.UpdateRoute("GET", "/hello", func(c *fiber.Ctx) error {
			return c.SendString("Hello, world! (updated)")
		})
		log.Println("Updated route /hello handler")
	}()

	// After 20 seconds, rename /hello to /hi.
	go func() {
		time.Sleep(20 * time.Second)
		dynRouter.RenameRoute("GET", "/hello", "/hi")
		log.Println("Renamed route /hello to /hi")
	}()

	// ----------------------
	// Group Routes
	// ----------------------
	// Create an API group with a common prefix and middleware.
	apiGroup := dynRouter.Group("/api", func(c *fiber.Ctx) error {
		log.Println("API group middleware executed")
		return dynRouter.Next(c)
	})
	// Add a group route.
	apiGroup.Get("/users", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"users": []string{"Alice", "Bob", "Charlie"}})
	})
	// Set a custom renderer for the /api/users route.
	// (Using the same HTML engine for demonstration.)
	dynRouter.SetRenderer("GET", "/api/users", engine)

	// After 30 seconds, remove the /api/users route.
	go func() {
		time.Sleep(30 * time.Second)
		dynRouter.RemoveRoute("GET", "/api/users")
		log.Println("Removed route /api/users")
	}()

	// ----------------------
	// Static Routes
	// ----------------------
	// Add a static route to serve assets from the "./public" directory.
	dynRouter.Static("/assets", "./static/dist", router.StaticConfig{
		Compress:     true,
		CacheControl: "public, max-age=3600",
	})

	// ----------------------
	// Renderer Route
	// ----------------------
	// Dynamically add a route that renders HTML using a custom renderer.
	go func() {
		time.Sleep(40 * time.Second)
		dynRouter.AddRoute("GET", "/renderer", func(c *fiber.Ctx) error {
			// This route uses the custom renderer (HTML engine) to render the "index" template.
			return c.Render("index", fiber.Map{"Title": "Dynamic Renderer"})
		})
		// Set the custom renderer for the route.
		dynRouter.SetRenderer("GET", "/renderer", engine)
		log.Println("Added new route /renderer with custom renderer")
	}()

	// Log registered routes for debugging.
	log.Println("Registered routes:", dynRouter.ListRoutes())

	// Start the server.
	log.Fatal(app.Listen(":3000"))
}
