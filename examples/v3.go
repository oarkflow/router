package main

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"

	"github.com/oarkflow/router"
)

func main() {
	engine := html.New("./static/dist", ".html")
	app := fiber.New(fiber.Config{
		Views: engine,
	})
	dynRouter := router.New(app)
	dynRouter.SetNotFoundHandler(func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Custom 404: Route not found",
		})
	})
	dynRouter.AddRoute("GET", "/hello", func(c *fiber.Ctx) error {
		return c.SendString("Hello, world!")
	})
	go func() {
		time.Sleep(10 * time.Second)
		dynRouter.UpdateRoute("GET", "/hello", func(c *fiber.Ctx) error {
			return c.SendString("Hello, world! (updated)")
		})
		log.Println("Updated normal route /hello")
	}()
	apiGroup := dynRouter.Group("/api", func(c *fiber.Ctx) error {
		log.Println("API group middleware executed")
		return router.Next(c)
	})
	apiGroup.Get("/users", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"users": []string{"Alice", "Bob", "Charlie"}})
	})
	v1Group := apiGroup.Group("/v1")
	v1Group.Get("/products", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"products": []string{"Laptop", "Phone", "Tablet"}})
	})
	go func() {
		time.Sleep(20 * time.Second)
		apiGroup.ChangePrefix("/v2")
		log.Println("Changed API group prefix to /v2")
	}()
	go func() {
		time.Sleep(30 * time.Second)
		apiGroup.UpdateMiddlewares([]fiber.Handler{
			func(c *fiber.Ctx) error {
				log.Println("New API group middleware executed")
				return router.Next(c)
			},
		})
		log.Println("Updated API group middleware")
	}()
	dynRouter.Static("/assets", "./static/dist", router.StaticConfig{
		Compress:     true,
		CacheControl: "public, max-age=3600",
	})
	go func() {
		time.Sleep(40 * time.Second)
		dynRouter.AddRoute("GET", "/renderer", func(c *fiber.Ctx) error {
			return c.Render("index", fiber.Map{"Title": "Dynamic Renderer"})
		})
		dynRouter.SetRenderer("GET", "/renderer", engine)
		log.Println("Added route /renderer with custom renderer")
	}()
	log.Println("Registered routes:", dynRouter.ListRoutes())
	log.Fatal(app.Listen(":3000"))
}
