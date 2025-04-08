package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"
	"github.com/oarkflow/json"

	"github.com/oarkflow/router"
	"github.com/oarkflow/router/utils"
)

type RendererConfig struct {
	ID        string `json:"id"`
	Root      string `json:"root"`
	Prefix    string `json:"prefix"`
	UseIndex  bool   `json:"use_index"`
	Compress  bool   `json:"compress"`
	Index     string `json:"index"`
	Extension string `json:"extension"`
}

type APISchema struct {
	RouteURI    string          `json:"route_uri"`
	RouteMethod string          `json:"route_method"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

type APIRoute struct {
	RouteURI    string            `json:"route_uri"`
	RouteMethod string            `json:"route_method"`
	Description string            `json:"description"`
	Model       string            `json:"model"`
	Operation   string            `json:"operation"`
	HandlerKey  string            `json:"handler_key"`
	Schema      json.RawMessage   `json:"schema,omitempty"`
	Rules       map[string]string `json:"rules,omitempty"`
}

type APIEndpoints struct {
	Prefix string     `json:"prefix"`
	Routes []APIRoute `json:"routes"`
}

var handlerMapping = map[string]fiber.Handler{
	"print:check": func(c *fiber.Ctx) error {
		var data map[string]any
		err := c.BodyParser(&data)
		if err != nil {
			return err
		}
		log.Println("print:check handler invoked", data)
		return c.SendString("print:check executed")
	},
	"view-html": func(c *fiber.Ctx) error {
		return c.Render("index", fiber.Map{"Title": "HTML View"})
	},
	"view-json": func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"message": "JSON View"})
	},
	"view-html-content": func(c *fiber.Ctx) error {
		c.Type("html")
		return c.SendString("<html><body><h1>HTML Content View</h1></body></html>")
	},
	"command:run": func(c *fiber.Ctx) error {
		return c.SendString("Command executed")
	},
}

var (
	dynamicRouter *router.Router
	app           *fiber.App
)

func init() {
	defaultEngine := html.New(utils.AbsPath("./static/dist"), ".html")
	app = fiber.New(fiber.Config{
		Views: defaultEngine,
	})
	app.Static("/public", utils.AbsPath("./public"))
	dynamicRouter = router.New(app)
	dynamicRouter.Use(dynamicRouter.ValidateRequestBySchema)
	initAPIEndpointsAndRenderer()
}

func loadSchemaFile(file string) error {
	schemaData, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("Could not read %s: %v", file, err)
	}
	err = loadSchemaBytes(schemaData)
	if err != nil {
		return err
	}
	return nil
}

func loadSchemaBytes(items json.RawMessage) error {
	var entries []APISchema
	if err := json.Unmarshal(items, &entries); err != nil {
		return err
	}
	for _, entry := range entries {
		router.CompileSchema(entry.RouteURI, entry.RouteMethod, entry.Schema)
	}
	return nil
}

func loadSchemas(file, dir string) error {
	err := loadSchemaFile(file)
	if err != nil {
		log.Println("Error loading schema file:", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Println("Error loading schema dir:", err)
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, file.Name())
		err := loadSchemaFile(path)
		if err != nil {
			return err
		}
	}
	return nil
}

func initAPIEndpointsAndRenderer() {
	err := loadSchemas(utils.AbsPath("./schema.json"), utils.AbsPath("./schemas"))
	if err != nil {
		log.Println("Error loading schemas:", err)
	} // load compiled schemas at startup
	dynamicRouter.SetNotFoundHandler(func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Custom 404: Route not found"})
	})
	rendererJSON, err := os.ReadFile(utils.AbsPath("./renderer.json"))
	if err != nil {
		log.Fatalf("Error reading renderer.json: %v", err) // changed from panic
	}
	var rendererConfigs []RendererConfig
	if err := json.Unmarshal(rendererJSON, &rendererConfigs); err != nil {
		log.Fatalf("Error parsing renderer JSON: %v", err)
	}
	for _, rc := range rendererConfigs {
		root := filepath.Clean(utils.AbsPath(rc.Root))
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if d.IsDir() {
				relativePath := strings.TrimPrefix(path, root)
				if relativePath != "" && !strings.HasPrefix(relativePath, "/") {
					relativePath = "/" + relativePath
				}
				if relativePath != "" {
					rootPath := filepath.Join(rc.Prefix, relativePath)
					dynamicRouter.Static(rootPath, path, router.StaticConfig{
						Compress:     true,
						CacheControl: "Cache-Control: public, max-age=86400",
					})
					dynamicRouter.Static(relativePath, path, router.StaticConfig{
						Compress:     true,
						CacheControl: "Cache-Control: public, max-age=86400",
					})
				}
			}
			return nil
		})
		if rc.UseIndex {
			customEngine := html.New(utils.AbsPath(rc.Root), rc.Extension)
			route := rc.Prefix
			dynamicRouter.AddRoute("GET", route, func(c *fiber.Ctx) error {
				c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
				return customEngine.Render(c, rc.Index, fiber.Map{
					"Title": "Custom Renderer - " + rc.ID,
				})
			})
			dynamicRouter.SetRenderer("GET", route, customEngine)
		}
	}
	apiBytes, err := os.ReadFile(utils.AbsPath("./api.json"))
	if err != nil {
		log.Fatalf("Error reading api.json: %v", err) // changed from panic
	}
	var apiConfig APIEndpoints
	if err := json.Unmarshal(apiBytes, &apiConfig); err != nil {
		log.Fatalf("Error parsing API routes JSON: %v", err)
	}
	for _, route := range apiConfig.Routes {
		var prefix string
		if apiConfig.Prefix != "" {
			prefix = "/" + strings.Trim(apiConfig.Prefix, "/")
		}
		path := prefix + "/" + strings.Trim(route.RouteURI, "/")
		handler, exists := handlerMapping[route.HandlerKey]
		if !exists {
			log.Printf("Handler not found for key: %s", route.HandlerKey)
			continue
		}
		if route.Schema != nil {
			router.CompileSchema(path, route.RouteMethod, route.Schema)
		}
		dynamicRouter.AddRoute(route.RouteMethod, route.RouteURI, handler)
	}
}

// ReloadRoutes reinitializes the dynamic routes, API endpoints, and schemas.
func ReloadRoutes() {
	log.Println("Reloading routes, schemas, and API endpoints...")
	// Clear all previously added routes.
	dynamicRouter.ClearRoutes()

	// Reload compiled schemas and API endpoints.
	initAPIEndpointsAndRenderer()

	// Re-register any additional dynamic routes (e.g. the "/hello" sample route).
	dynamicRouter.AddRoute("GET", "/hello", func(c *fiber.Ctx) error {
		return c.SendString("Hello from the dynamic router!")
	})

	// Ensure the reload endpoint is registered.
	dynamicRouter.AddRoute("GET", "/reload", reloadHandler)

	log.Println("Routes reloaded. Registered routes:", dynamicRouter.ListRoutes())
}

// reloadHandler is an HTTP handler that triggers a reload.
func reloadHandler(c *fiber.Ctx) error {
	ReloadRoutes()
	return c.SendString("Routes reloaded")
}

func main() {

	dynamicRouter.AddRoute("GET", "/hello", func(c *fiber.Ctx) error {
		return c.SendString("Hello from the dynamic router!")
	})
	// Register the reload endpoint.
	dynamicRouter.AddRoute("POST", "/reload", reloadHandler)
	go func() {
		time.Sleep(5 * time.Second)
		dynamicRouter.UpdateRoute("GET", "/hello", func(c *fiber.Ctx) error {
			return c.SendString("Updated handler response!")
		})
	}()
	go func() {
		time.Sleep(10 * time.Second)
		dynamicRouter.RenameRoute("GET", "/hello", "/greetings")
	}()
	// Add graceful shutdown support:
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, os.Interrupt)
		<-quit
		log.Println("Shutting down gracefully...")
		if err := app.Shutdown(); err != nil {
			log.Fatalf("Shutdown error: %v", err)
		}
	}()
	log.Println("Registered routes:", dynamicRouter.ListRoutes())
	log.Fatal(app.Listen(":3000"))
}
