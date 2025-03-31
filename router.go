package router

import (
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v2"

	"github.com/oarkflow/router/utils"
)

type Route struct {
	Method      string
	Path        string
	Handler     fiber.Handler
	Middlewares []fiber.Handler
	Renderer    fiber.Views
}

func (dr *Route) Serve(c *fiber.Ctx, router *Router, globalMWs ...fiber.Handler) error {
	chain := make([]fiber.Handler, 0, len(globalMWs)+len(dr.Middlewares)+1)
	chain = append(chain, globalMWs...)
	chain = append(chain, dr.Middlewares...)
	chain = append(chain, dr.Handler)
	c.Locals("chain_handlers", chain)
	c.Locals("chain_index", 0)
	if err := router.Next(c); err != nil {
		return err
	}
	body := c.Response().Body()
	if len(body) > 0 {
		compData, err := compressData(c, body)
		if err != nil {
			return err
		}
		c.Response().SetBodyRaw(compData)
	}
	return nil
}

func compressData(c *fiber.Ctx, data []byte) ([]byte, error) {
	acceptEncoding := c.Get("Accept-Encoding")
	if strings.Contains(acceptEncoding, "br") {
		if compressed, err := utils.CompressBrotli(data); err == nil {
			c.Response().Header.Set("Content-Encoding", "br")
			return compressed, nil
		}
	}
	if strings.Contains(acceptEncoding, "gzip") {
		if compressed, err := utils.CompressGzip(data); err == nil {
			c.Response().Header.Set("Content-Encoding", "gzip")
			return compressed, nil
		}
	}
	return data, nil
}

type Static struct {
	Prefix       string `json:"prefix"`
	Directory    string `json:"directory"`
	CacheControl string // Added field for Cache-Control header
}

type Router struct {
	app               *fiber.App
	routes            map[string]map[string]*Route
	staticRoutes      []Static
	GlobalMiddlewares []fiber.Handler
	lock              sync.RWMutex
	NotFoundHandler   fiber.Handler

	// Enhancement: cache for static file contents
	staticCache map[string][]byte
}

func New(app *fiber.App) *Router {
	dr := &Router{
		app:         app,
		routes:      make(map[string]map[string]*Route),
		staticCache: make(map[string][]byte), // initialize cache
	}
	app.All("/*", dr.dispatch)
	return dr
}

func (dr *Router) Next(c *fiber.Ctx) error {
	idxVal := c.Locals("chain_index")
	idx, ok := idxVal.(int)
	if !ok {
		idx = 0
	}
	handlersVal := c.Locals("chain_handlers")
	handlers, ok := handlersVal.([]fiber.Handler)
	if !ok || idx >= len(handlers) {
		return nil
	}
	c.Locals("chain_index", idx+1)
	return handlers[idx](c)
}

func (dr *Router) Use(mw ...fiber.Handler) {
	dr.GlobalMiddlewares = append(dr.GlobalMiddlewares, mw...)
}

func (dr *Router) dispatch(c *fiber.Ctx) error {
	dr.lock.RLock()
	defer dr.lock.RUnlock()
	method := c.Method()
	path := c.Path()
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			return route.Serve(c, dr, dr.GlobalMiddlewares...)
		}
	}
	for _, sr := range dr.staticRoutes {
		if strings.HasPrefix(path, sr.Prefix) {
			relativePath := strings.TrimPrefix(path, sr.Prefix)
			filePath := filepath.Join(sr.Directory, relativePath)
			if info, err := os.Stat(filePath); err == nil && info.IsDir() {
				filePath = filepath.Join(filePath, "index.html")
			}
			if _, err := os.Stat(filePath); err == nil {
				if mimeType := mime.TypeByExtension(filepath.Ext(filePath)); mimeType != "" {
					c.Response().Header.Set("Content-Type", mimeType)
				}
				if sr.CacheControl != "" {
					c.Response().Header.Set("Cache-Control", sr.CacheControl)
				}
				var data []byte
				if cached, ok := dr.staticCache[filePath]; ok {
					data = cached
					log.Printf("Cache hit for file: %s", filePath)
				} else {
					d, err := os.ReadFile(filePath)
					if err != nil {
						return c.Status(500).SendString("Error reading file")
					}
					data = d
					dr.staticCache[filePath] = data
				}
				compData, err := compressData(c, data)
				if err != nil {
					return err
				}
				return c.Send(compData)
			}
		}
	}
	if dr.NotFoundHandler != nil {
		return dr.NotFoundHandler(c)
	}
	return c.Status(fiber.StatusNotFound).SendString("Dynamic route not found")
}

func (dr *Router) AddRoute(method, path string, handler fiber.Handler, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if dr.routes[method] == nil {
		dr.routes[method] = make(map[string]*Route)
	}
	dr.routes[method][path] = &Route{
		Method:      method,
		Path:        path,
		Handler:     handler,
		Middlewares: middlewares,
	}
	log.Printf("Added dynamic route: %s %s", method, path)
}

func (dr *Router) UpdateRoute(method, path string, newHandler fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			route.Handler = newHandler
			log.Printf("Updated dynamic route handler: %s %s", method, path)
			return
		}
	}
	log.Printf("Route not found for update: %s %s", method, path)
}

func (dr *Router) RenameRoute(method, oldPath, newPath string) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[oldPath]; exists {
			delete(methodRoutes, oldPath)
			route.Path = newPath
			methodRoutes[newPath] = route
			log.Printf("Renamed route from %s to %s for method %s", oldPath, newPath, method)
			return
		}
	}
	log.Printf("Route not found for rename: %s %s", method, oldPath)
}

func (dr *Router) AddMiddleware(method, path string, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			route.Middlewares = append(route.Middlewares, middlewares...)
			log.Printf("Added middleware to route: %s %s", method, path)
			return
		}
	}
	log.Printf("Route not found for adding middleware: %s %s", method, path)
}

func (dr *Router) RemoveMiddleware(method, path string, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			newChain := make([]fiber.Handler, 0, len(route.Middlewares))
			for _, existing := range route.Middlewares {
				shouldRemove := false
				for _, rm := range middlewares {
					if &existing == &rm {
						shouldRemove = true
						break
					}
				}
				if !shouldRemove {
					newChain = append(newChain, existing)
				}
			}
			route.Middlewares = newChain
			log.Printf("Removed middleware from route: %s %s", method, path)
			return
		}
	}
	log.Printf("Route not found for removing middleware: %s %s", method, path)
}

func (dr *Router) SetRenderer(method, path string, renderer fiber.Views) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			route.Renderer = renderer
			log.Printf("Set custom renderer for route: %s %s", method, path)
			return
		}
	}
	log.Printf("Route not found for setting renderer: %s %s", method, path)
}

type StaticConfig struct {
	Compress     bool
	ByteRange    bool
	CacheControl string
}

func (dr *Router) Static(prefix, directory string, cfg ...StaticConfig) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	cacheControl := ""
	if len(cfg) > 0 {
		cacheControl = cfg[0].CacheControl
	}
	dr.staticRoutes = append(dr.staticRoutes, Static{
		Prefix:       prefix,
		Directory:    directory,
		CacheControl: cacheControl,
	})
	log.Printf("Added static route: %s -> %s", prefix, directory)
}

func (dr *Router) RemoveRoute(method, path string) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if _, exists := methodRoutes[path]; exists {
			delete(methodRoutes, path)
			log.Printf("Removed dynamic route: %s %s", method, path)
			return
		}
	}
	log.Printf("Route not found for removal: %s %s", method, path)
}

func (dr *Router) SetNotFoundHandler(handler fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	dr.NotFoundHandler = handler
	log.Println("Custom NotFoundHandler set")
}

func (dr *Router) ListRoutes() []string {
	dr.lock.RLock()
	defer dr.lock.RUnlock()
	var routesList []string
	for method, routes := range dr.routes {
		for path := range routes {
			routesList = append(routesList, method+" "+path)
		}
	}
	return routesList
}
