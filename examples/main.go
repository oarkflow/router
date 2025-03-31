package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"

	"github.com/oarkflow/router"
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

type APIRoute struct {
	RouteURI    string                 `json:"route_uri"`
	RouteMethod string                 `json:"route_method"`
	Description string                 `json:"description"`
	Model       string                 `json:"model"`
	Operation   string                 `json:"operation"`
	HandlerKey  string                 `json:"handler_key"`
	Request     map[string]interface{} `json:"request,omitempty"`
}

type APIEndpoints struct {
	Routes []APIRoute `json:"routes"`
}

var handlerMapping = map[string]fiber.Handler{
	"print:check": func(c *fiber.Ctx) error {
		log.Println("print:check handler invoked")
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

func main() {
	defaultEngine := html.New("./static/dist", ".html")
	app := fiber.New(fiber.Config{
		Views: defaultEngine,
	})
	app.Static("/public", "./public")

	dynamicRouter := router.New(app)
	rendererJSON, err := os.ReadFile("renderer.json")
	if err != nil {
		panic(err)
	}
	var rendererConfigs []RendererConfig
	if err := json.Unmarshal(rendererJSON, &rendererConfigs); err != nil {
		log.Fatalf("Error parsing renderer JSON: %v", err)
	}
	for _, rc := range rendererConfigs {
		root := filepath.Clean(rc.Root)
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if d.IsDir() {
				relativePath := strings.TrimPrefix(path, root)
				if relativePath != "" && !strings.HasPrefix(relativePath, "/") {
					relativePath = "/" + relativePath
				}
				if relativePath != "" {
					rootPath := filepath.Join(rc.Prefix, relativePath)
					dynamicRouter.AddStaticRoute(rootPath, path)
					dynamicRouter.AddStaticRoute(relativePath, path)
				}
			}
			return nil
		})
		if rc.UseIndex {
			customEngine := html.New(rc.Root, rc.Extension)
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
	apiBytes, err := os.ReadFile("api.json")
	if err != nil {
		panic(err)
	}
	var apiConfig APIEndpoints
	if err := json.Unmarshal(apiBytes, &apiConfig); err != nil {
		log.Fatalf("Error parsing API routes JSON: %v", err)
	}
	for _, route := range apiConfig.Routes {
		handler, exists := handlerMapping[route.HandlerKey]
		if !exists {
			log.Printf("Handler not found for key: %s", route.HandlerKey)
			continue
		}
		dynamicRouter.AddRoute(route.RouteMethod, route.RouteURI, handler)
	}
	dynamicRouter.AddRoute("GET", "/hello", func(c *fiber.Ctx) error {
		return c.SendString("Hello from the dynamic router!")
	})
	go func() {
		time.Sleep(30 * time.Second)
		dynamicRouter.UpdateRoute("GET", "/hello", func(c *fiber.Ctx) error {
			return c.SendString("Updated handler response!")
		})
	}()
	go func() {
		time.Sleep(40 * time.Second)
		dynamicRouter.RenameRoute("GET", "/hello", "/greetings")
	}()
	log.Fatal(app.Listen(":3000"))
}
