package router

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	
	"github.com/gofiber/fiber/v2"
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
	
	return router.Next(c)
}

type Static struct {
	Prefix    string `json:"prefix"`
	Directory string `json:"directory"`
}

type Router struct {
	app               *fiber.App
	routes            map[string]map[string]*Route
	staticRoutes      []Static
	GlobalMiddlewares []fiber.Handler
	lock              sync.RWMutex
	NotFoundHandler   fiber.Handler
}

func New(app *fiber.App) *Router {
	dr := &Router{
		app:    app,
		routes: make(map[string]map[string]*Route),
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
			if _, err := os.Stat(filePath); err == nil {
				return c.SendFile(filePath)
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
	dr.staticRoutes = append(dr.staticRoutes, Static{
		Prefix:    prefix,
		Directory: directory,
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
