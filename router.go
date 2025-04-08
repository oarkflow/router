package router

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/oarkflow/log"

	"github.com/oarkflow/router/utils"
)

type middlewareEntry struct {
	id      uintptr
	handler fiber.Handler
}

func wrapMiddleware(m fiber.Handler) middlewareEntry {
	return middlewareEntry{
		id:      reflect.ValueOf(m).Pointer(),
		handler: m,
	}
}

func middlewareIDsEqual(a fiber.Handler, b middlewareEntry) bool {
	return reflect.ValueOf(a).Pointer() == b.id
}

// ErrorHandler handles errors for HTTP requests.
var (
	// ErrorHandler handles errors for HTTP requests.
	ErrorHandler func(c *fiber.Ctx, err error) error
	// SuccessHandler sends successful JSON responses.
	SuccessHandler func(c *fiber.Ctx, data any) error
)

func init() {
	ErrorHandler = func(c *fiber.Ctx, err error) error {
		resp := map[string]any{
			"success": false,
			"message": err.Error(),
			"code":    fiber.StatusInternalServerError,
		}
		return c.JSON(resp)
	}
	SuccessHandler = func(c *fiber.Ctx, data any) error {
		resp := map[string]any{
			"success": true,
			"data":    data,
			"code":    fiber.StatusOK,
		}
		return c.JSON(resp)
	}
}

// Route represents a dynamic route.
type Route struct {
	// Method is the HTTP method for the route.
	Method string
	// Path is the URL pattern for the route.
	Path string
	// Handler is the function that handles the route.
	Handler fiber.Handler
	// Middlewares are the route-specific middlewares.
	Middlewares []middlewareEntry
	// Renderer is used to render the response.
	Renderer fiber.Views
}

// Serve executes the route's handler chain.
func (dr *Route) Serve(c *fiber.Ctx) error {
	chain := make([]fiber.Handler, 0, len(dr.Middlewares)+1)
	for _, m := range dr.Middlewares {
		chain = append(chain, m.handler)
	}
	chain = append(chain, dr.Handler)
	c.Locals("chain_handlers", chain)
	c.Locals("chain_index", 0)
	if err := Next(c); err != nil {
		return fmt.Errorf("chain error: %w", err)
	}
	body := c.Response().Body()
	if len(body) > 0 {
		compData, err := compressData(c, body)
		if err != nil {
			return fmt.Errorf("compression error: %w", err)
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

type methodRoutes struct {
	mu     sync.RWMutex
	exact  map[string]*Route
	params []*Route
}

type Static struct {
	Prefix           string `json:"prefix"`
	Directory        string `json:"directory"`
	CacheControl     string
	DirectoryListing bool
	CompressionLevel int
}

type staticCacheEntry struct {
	data      []byte
	timestamp time.Time
}

const staticCacheTTL = 5 * time.Minute

type StaticConfig struct {
	Compress         bool
	ByteRange        bool
	CacheControl     string
	DirectoryListing bool
	CompressionLevel int
}

// Router represents the HTTP router.
type Router struct {
	app          *fiber.App
	routes       sync.Map // key: string (HTTP method) -> *methodRoutes
	staticRoutes []Static
	// GlobalMiddlewares are applied to every route.
	GlobalMiddlewares []middlewareEntry
	// NotFoundHandler is invoked when no route matches.
	NotFoundHandler fiber.Handler
	staticCache     map[string]staticCacheEntry
	staticCacheLock sync.RWMutex
}

// New creates and returns a new Router instance.
func New(app *fiber.App) *Router {
	dr := &Router{
		app:         app,
		staticCache: make(map[string]staticCacheEntry),
	}

	app.Use(func(c *fiber.Ctx) error {
		err := c.Next()
		if err != nil {
			if ErrorHandler != nil {
				return ErrorHandler(c, err)
			}
			return err
		}
		return nil
	})
	app.All("/*", dr.dispatch)
	return dr
}

// Next executes the next handler in the middleware chain.
func Next(c *fiber.Ctx) error {
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
	if err := handlers[idx](c); err != nil {
		return err
	}
	return nil
}

// GetGlobalMiddlewareChain returns the global middleware chain.
func (dr *Router) GetGlobalMiddlewareChain() []fiber.Handler {
	chain := make([]fiber.Handler, len(dr.GlobalMiddlewares))
	for i, m := range dr.GlobalMiddlewares {
		chain[i] = m.handler
	}
	return chain
}

// UpdateGlobalMiddleware updates the router's global middleware chain.
func (dr *Router) UpdateGlobalMiddleware(newMW []fiber.Handler) {
	var newChain []middlewareEntry
	for _, m := range newMW {
		newChain = append(newChain, wrapMiddleware(m))
	}
	dr.GlobalMiddlewares = newChain
	log.Info().Int("count", len(newMW)).Msg("Updated global middleware")
}

// Use adds new global middlewares.
func (dr *Router) Use(mw ...fiber.Handler) {
	for _, m := range mw {
		dr.GlobalMiddlewares = append(dr.GlobalMiddlewares, wrapMiddleware(m))
	}
	log.Info().Int("count", len(mw)).Msg("Added to global middleware")
}

// MatchRoute finds and returns a matching dynamic route.
func (dr *Router) MatchRoute(method, path string) (*Route, bool, map[string]string) {
	if v, ok := dr.routes.Load(method); ok {
		mr := v.(*methodRoutes)
		mr.mu.RLock()
		defer mr.mu.RUnlock()
		if route, exists := mr.exact[path]; exists {
			return route, true, nil
		}
		for _, route := range mr.params {
			if matched, params := utils.MatchRoute(route.Path, path); matched {
				return route, true, params
			}
		}
	}
	return nil, false, nil
}

func (dr *Router) dispatch(c *fiber.Ctx) error {
	globalChain := dr.GetGlobalMiddlewareChain()
	nextFunc := func(c *fiber.Ctx) error {
		method := c.Method()
		path := c.Path()
		route, matched, params := dr.MatchRoute(method, path)
		if matched {
			if params != nil {
				c.Locals("params", params)
			}
			return route.Serve(c)
		}
		for _, sr := range dr.staticRoutes {
			if strings.HasPrefix(path, sr.Prefix) {
				relativePath := strings.TrimPrefix(path, sr.Prefix)
				cleanRelative := filepath.Clean(relativePath)
				filePath := filepath.Join(sr.Directory, cleanRelative)
				absDir, err := filepath.Abs(sr.Directory)
				if err != nil {
					log.Error().Err(err).Msg("Could not resolve absolute directory")
					return c.Status(500).SendString("Internal Server Error")
				}
				absFile, err := filepath.Abs(filePath)
				if err != nil || !strings.HasPrefix(absFile, absDir) {
					log.Warn().Err(err).Msgf("Attempted directory traversal: %s", filePath)
					return c.Status(403).SendString("Forbidden")
				}
				info, err := os.Stat(filePath)
				if err == nil && info.IsDir() {
					if sr.DirectoryListing {
						entries, err := os.ReadDir(filePath)
						if err != nil {
							log.Error().Err(err).Msgf("Failed to read directory: %s", filePath)
							return c.Status(500).SendString("Error reading directory")
						}
						var builder strings.Builder
						builder.WriteString("<html><head><meta charset=\"UTF-8\"><title>Directory listing</title></head><body>")
						builder.WriteString("<h1>Directory listing for " + c.Path() + "</h1><ul>")
						for _, entry := range entries {
							name := entry.Name()
							entryLink := filepath.Join(c.Path(), name)
							builder.WriteString(fmt.Sprintf("<li><a href=\"%s\">%s</a></li>", entryLink, name))
						}
						builder.WriteString("</ul></body></html>")
						data := []byte(builder.String())
						c.Response().Header.Set("Content-Type", "text/html")
						return c.Send(data)
					}
					filePath = filepath.Join(filePath, "index.html")
				}
				if _, err := os.Stat(filePath); err == nil {
					ext := filepath.Ext(filePath)
					if mimeType := mime.TypeByExtension(ext); mimeType != "" {
						c.Response().Header.Set("Content-Type", mimeType)
						c.Response().Header.Set("X-Content-Type-Options", "nosniff")
					}
					if sr.CacheControl != "" {
						c.Response().Header.Set("Cache-Control", sr.CacheControl)
					}
					var data []byte
					dr.staticCacheLock.RLock()
					entry, found := dr.staticCache[filePath]
					dr.staticCacheLock.RUnlock()
					if found && time.Since(entry.timestamp) < staticCacheTTL {
						data = entry.data
						log.Info().Str("file", filePath).Msg("Static cache hit")
					} else {
						d, err := os.ReadFile(filePath)
						if err != nil {
							log.Error().Err(err).Msgf("Failed to read file: %s", filePath)
							return c.Status(500).SendString("Error reading file")
						}
						data = d
						dr.staticCacheLock.Lock()
						dr.staticCache[filePath] = staticCacheEntry{data: data, timestamp: time.Now()}
						dr.staticCacheLock.Unlock()
					}
					compData, err := compressData(c, data)
					if err != nil {
						log.Error().Err(err).Msgf("Failed to compress file: %s", filePath)
						return c.Status(500).SendString("Compression error")
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

	chain := append(globalChain, nextFunc)
	c.Locals("chain_handlers", chain)
	c.Locals("chain_index", 0)
	return Next(c)
}

// AddRoute adds a new dynamic route.
func (dr *Router) AddRoute(method, path string, handler fiber.Handler, middlewares ...fiber.Handler) {
	method = strings.ToUpper(method)
	var mr *methodRoutes
	if v, ok := dr.routes.Load(method); !ok {
		mr = &methodRoutes{
			exact:  make(map[string]*Route),
			params: []*Route{},
		}
		dr.routes.Store(method, mr)
	} else {
		mr = v.(*methodRoutes)
	}
	mr.mu.Lock()
	defer mr.mu.Unlock()

	var mwEntries []middlewareEntry
	for _, m := range middlewares {
		mwEntries = append(mwEntries, wrapMiddleware(m))
	}
	route := &Route{
		Method:      method,
		Path:        path,
		Handler:     handler,
		Middlewares: mwEntries,
	}
	if strings.Contains(path, ":") {
		mr.params = append(mr.params, route)
	} else {
		mr.exact[path] = route
	}
	log.Info().Str("method", method).Str("path", path).Msg("Added dynamic route")
}

// UpdateRoute updates the handler of an existing route.
func (dr *Router) UpdateRoute(method, path string, newHandler fiber.Handler) {
	method = strings.ToUpper(method)
	if v, ok := dr.routes.Load(method); ok {
		mr := v.(*methodRoutes)
		mr.mu.Lock()
		defer mr.mu.Unlock()
		if route, exists := mr.exact[path]; exists {
			route.Handler = newHandler
			log.Info().Str("method", method).Str("path", path).Msg("Updated dynamic route handler")
			return
		}
		for _, route := range mr.params {
			if route.Path == path {
				route.Handler = newHandler
				log.Info().Str("method", method).Str("path", path).Msg("Updated dynamic route handler")
				return
			}
		}
	}
	log.Warn().Str("method", method).Str("path", path).Msg("Route not found for update")
}

// RenameRoute renames an existing dynamic route.
func (dr *Router) RenameRoute(method, oldPath, newPath string) {
	method = strings.ToUpper(method)
	if v, ok := dr.routes.Load(method); ok {
		mr := v.(*methodRoutes)
		mr.mu.Lock()
		defer mr.mu.Unlock()
		if route, exists := mr.exact[oldPath]; exists {
			delete(mr.exact, oldPath)
			route.Path = newPath
			if strings.Contains(newPath, ":") {
				mr.params = append(mr.params, route)
			} else {
				mr.exact[newPath] = route
			}
			log.Info().Str("method", method).Str("oldPath", oldPath).Str("newPath", newPath).Msg("Renamed route")
			return
		}
		for i, route := range mr.params {
			if route.Path == oldPath {
				mr.params = append(mr.params[:i], mr.params[i+1:]...)
				route.Path = newPath
				if strings.Contains(newPath, ":") {
					mr.params = append(mr.params, route)
				} else {
					mr.exact[newPath] = route
				}
				log.Info().Str("method", method).Str("oldPath", oldPath).Str("newPath", newPath).Msg("Renamed route")
				return
			}
		}
	}
	log.Warn().Str("method", method).Str("oldPath", oldPath).Str("newPath", newPath).Msg("Route not found for rename")
}

// AddMiddleware adds middleware to an existing route.
func (dr *Router) AddMiddleware(method, path string, middlewares ...fiber.Handler) {
	method = strings.ToUpper(method)
	if v, ok := dr.routes.Load(method); ok {
		mr := v.(*methodRoutes)
		mr.mu.Lock()
		defer mr.mu.Unlock()
		if route, exists := mr.exact[path]; exists {
			for _, m := range middlewares {
				route.Middlewares = append(route.Middlewares, wrapMiddleware(m))
			}
			log.Info().Str("method", method).Str("path", path).Int("count", len(middlewares)).Msg("Added middleware to route")
			return
		}
		for _, route := range mr.params {
			if route.Path == path {
				for _, m := range middlewares {
					route.Middlewares = append(route.Middlewares, wrapMiddleware(m))
				}
				log.Info().Str("method", method).Str("path", path).Int("count", len(middlewares)).Msg("Added middleware to route")
				return
			}
		}
	}
	log.Warn().Str("method", method).Str("path", path).Int("count", len(middlewares)).Msg("Route not found for adding middleware")
}

// RemoveMiddleware removes middleware from a route.
func (dr *Router) RemoveMiddleware(method, path string, middlewares ...fiber.Handler) {
	method = strings.ToUpper(method)
	if v, ok := dr.routes.Load(method); ok {
		mr := v.(*methodRoutes)
		mr.mu.Lock()
		defer mr.mu.Unlock()
		removeFromRoute := func(route *Route) {
			newChain := make([]middlewareEntry, 0, len(route.Middlewares))
			for _, existing := range route.Middlewares {
				shouldRemove := false
				for _, rm := range middlewares {
					if middlewareIDsEqual(rm, existing) {
						shouldRemove = true
						break
					}
				}
				if !shouldRemove {
					newChain = append(newChain, existing)
				}
			}
			route.Middlewares = newChain
		}
		if route, exists := mr.exact[path]; exists {
			removeFromRoute(route)
			log.Info().Str("method", method).Str("path", path).Int("count", len(middlewares)).Msg("Removed middleware from route")
			return
		}
		for _, route := range mr.params {
			if route.Path == path {
				removeFromRoute(route)
				log.Info().Str("method", method).Str("path", path).Int("count", len(middlewares)).Msg("Removed middleware from route")
				return
			}
		}
	}
	log.Warn().Str("method", method).Str("path", path).Int("count", len(middlewares)).Msg("Route not found for removing middleware")
}

// SetRenderer sets a custom renderer for a dynamic route.
func (dr *Router) SetRenderer(method, path string, renderer fiber.Views) {
	method = strings.ToUpper(method)
	if v, ok := dr.routes.Load(method); ok {
		mr := v.(*methodRoutes)
		mr.mu.Lock()
		defer mr.mu.Unlock()
		if route, exists := mr.exact[path]; exists {
			route.Renderer = renderer
			log.Info().Str("method", method).Str("path", path).Msg("Set custom renderer for route")
			return
		}
		for _, route := range mr.params {
			if route.Path == path {
				route.Renderer = renderer
				log.Info().Str("method", method).Str("path", path).Msg("Set custom renderer for route")
				return
			}
		}
	}
	log.Warn().Str("method", method).Str("path", path).Msg("Route not found for setting renderer")
}

// Static adds a new static route.
func (dr *Router) Static(prefix, directory string, cfg ...StaticConfig) {
	cacheControl := ""
	var sc StaticConfig
	if len(cfg) > 0 {
		sc = cfg[0]
		cacheControl = sc.CacheControl
	}
	dr.staticRoutes = append(dr.staticRoutes, Static{
		Prefix:           prefix,
		Directory:        directory,
		CacheControl:     cacheControl,
		DirectoryListing: sc.DirectoryListing,
		CompressionLevel: sc.CompressionLevel,
	})
	log.Info().Str("prefix", prefix).Str("directory", directory).Msg("Added static route")
}

// RemoveRoute deletes an existing dynamic route.
func (dr *Router) RemoveRoute(method, path string) {
	method = strings.ToUpper(method)
	if v, ok := dr.routes.Load(method); ok {
		mr := v.(*methodRoutes)
		mr.mu.Lock()
		defer mr.mu.Unlock()
		if _, exists := mr.exact[path]; exists {
			delete(mr.exact, path)
			log.Info().Str("method", method).Str("path", path).Msg("Removed dynamic route")
			return
		}
		for i, route := range mr.params {
			if route.Path == path {
				mr.params = append(mr.params[:i], mr.params[i+1:]...)
				log.Info().Str("method", method).Str("path", path).Msg("Removed dynamic route")
				return
			}
		}
	}
	log.Warn().Str("method", method).Str("path", path).Msg("Route not found for removal")
}

// SetNotFoundHandler sets the custom NotFound handler.
func (dr *Router) SetNotFoundHandler(handler fiber.Handler) {
	dr.NotFoundHandler = handler
	log.Info().Msg("Set custom NotFoundHandler")
}

// ListRoutes returns a list of all registered dynamic routes.
func (dr *Router) ListRoutes() []string {
	var routesList []string
	dr.routes.Range(func(key, value interface{}) bool {
		method := key.(string)
		mr := value.(*methodRoutes)
		mr.mu.RLock()
		for path := range mr.exact {
			routesList = append(routesList, method+" "+path)
		}
		for _, route := range mr.params {
			routesList = append(routesList, method+" "+route.Path)
		}
		mr.mu.RUnlock()
		return true
	})
	return routesList
}

// InvalidateStaticCache invalidates the cache for a static file.
func (dr *Router) InvalidateStaticCache(file string) {
	dr.staticCacheLock.Lock()
	defer dr.staticCacheLock.Unlock()
	delete(dr.staticCache, file)
	log.Info().Str("file", file).Msg("Invalidated static cache")
}

// Shutdown initiates a graceful shutdown of the router.
func (dr *Router) Shutdown() error {
	log.Info().Msg("Initiating graceful shutdown")
	return dr.app.Shutdown()
}

// GroupRoute represents a route defined within a group.
type GroupRoute struct {
	// method is the HTTP method.
	method string
	// relPath is the relative path for the route.
	relPath string
	// handler is the function to handle the route.
	handler fiber.Handler
	// routeMWs are the group-specific middlewares for the route.
	routeMWs []middlewareEntry
	// effectivePath is the complete route path.
	effectivePath string
}

// Group represents a collection of routes with a common prefix and middlewares.
type Group struct {
	// prefix is the base URL segment for the group.
	prefix string
	// middlewares are applied to all routes in the group.
	middlewares []middlewareEntry
	// routes are the routes belonging to the group.
	routes []*GroupRoute
	router *Router
}

// Group creates a new subgroup with an additional prefix.
func (g *Group) Group(prefix string, m ...fiber.Handler) *Group {
	newPrefix := g.prefix + prefix
	newMW := make([]middlewareEntry, len(g.middlewares))
	copy(newMW, g.middlewares)
	for _, mw := range m {
		newMW = append(newMW, wrapMiddleware(mw))
	}
	return &Group{
		prefix:      newPrefix,
		middlewares: newMW,
		router:      g.router,
	}
}

// AddRoute adds a new route to the group.
func (g *Group) AddRoute(method, relPath string, handler fiber.Handler, m ...fiber.Handler) {
	effectivePath := g.prefix + relPath
	var routeMWs []middlewareEntry
	for _, mw := range m {
		routeMWs = append(routeMWs, wrapMiddleware(mw))
	}
	gr := &GroupRoute{
		method:        strings.ToUpper(method),
		relPath:       relPath,
		handler:       handler,
		routeMWs:      routeMWs,
		effectivePath: effectivePath,
	}
	g.routes = append(g.routes, gr)
	combinedMW := make([]fiber.Handler, 0, len(g.middlewares)+len(routeMWs))
	for _, m := range g.middlewares {
		combinedMW = append(combinedMW, m.handler)
	}
	for _, m := range routeMWs {
		combinedMW = append(combinedMW, m.handler)
	}
	g.router.AddRoute(method, effectivePath, handler, combinedMW...)
}

// Get adds a new GET route to the group.
func (g *Group) Get(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("GET", relPath, handler, m...)
}

// Post adds a new POST route to the group.
func (g *Group) Post(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("POST", relPath, handler, m...)
}

// Put adds a new PUT route to the group.
func (g *Group) Put(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("PUT", relPath, handler, m...)
}

// Delete adds a new DELETE route to the group.
func (g *Group) Delete(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("DELETE", relPath, handler, m...)
}

// Patch adds a new PATCH route to the group.
func (g *Group) Patch(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("PATCH", relPath, handler, m...)
}

// Options adds a new OPTIONS route to the group.
func (g *Group) Options(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("OPTIONS", relPath, handler, m...)
}

// Head adds a new HEAD route to the group.
func (g *Group) Head(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("HEAD", relPath, handler, m...)
}

// Static adds a new static route within the group.
func (g *Group) Static(prefix, directory string, cfg ...StaticConfig) {
	fullPrefix := g.prefix + prefix
	g.router.Static(fullPrefix, directory, cfg...)
}

// ChangePrefix updates the group's prefix and the effective path of its routes.
func (g *Group) ChangePrefix(newPrefix string) {
	oldPrefix := g.prefix
	if oldPrefix == newPrefix {
		return
	}
	g.prefix = newPrefix
	for _, gr := range g.routes {
		oldEffective := gr.effectivePath
		newEffective := newPrefix + gr.relPath
		g.router.RenameRoute(gr.method, oldEffective, newEffective)
		gr.effectivePath = newEffective
	}
	log.Info().Str("oldPrefix", oldPrefix).Str("newPrefix", newPrefix).Msg("Group prefix changed")
}

// UpdateMiddlewares updates the group's middlewares.
func (g *Group) UpdateMiddlewares(newMW []fiber.Handler) {
	var newWrapped []middlewareEntry
	for _, m := range newMW {
		newWrapped = append(newWrapped, wrapMiddleware(m))
	}
	g.middlewares = newWrapped
	for _, gr := range g.routes {
		g.router.RemoveRoute(gr.method, gr.effectivePath)
		combinedMW := make([]fiber.Handler, 0, len(g.middlewares)+len(gr.routeMWs))
		for _, m := range g.middlewares {
			combinedMW = append(combinedMW, m.handler)
		}
		for _, m := range gr.routeMWs {
			combinedMW = append(combinedMW, m.handler)
		}
		g.router.AddRoute(gr.method, gr.effectivePath, gr.handler, combinedMW...)
	}
	log.Info().Str("groupPrefix", g.prefix).Msg("Group middlewares updated")
}

// AddMiddleware adds new middleware to the group.
func (g *Group) AddMiddleware(mw ...fiber.Handler) {
	current := make([]fiber.Handler, 0, len(g.middlewares))
	for _, m := range g.middlewares {
		current = append(current, m.handler)
	}
	current = append(current, mw...)
	g.UpdateMiddlewares(current)
}

// RemoveMiddleware removes middleware from the group.
func (g *Group) RemoveMiddleware(mw ...fiber.Handler) {
	var newMW []fiber.Handler
	for _, m := range g.middlewares {
		keep := true
		for _, rm := range mw {
			if reflect.ValueOf(m.handler).Pointer() == reflect.ValueOf(rm).Pointer() {
				keep = false
				break
			}
		}
		if keep {
			newMW = append(newMW, m.handler)
		}
	}
	g.UpdateMiddlewares(newMW)
}

// RemoveRoute removes a route from the group.
func (g *Group) RemoveRoute(relPath string) {
	for i, gr := range g.routes {
		if gr.relPath == relPath {
			g.router.RemoveRoute(gr.method, gr.effectivePath)
			g.routes = append(g.routes[:i], g.routes[i+1:]...)
			log.Info().Str("groupPrefix", g.prefix).Str("relPath", relPath).Msg("Group route removed")
			return
		}
	}
	log.Warn().Str("relPath", relPath).Msg("Group route not found for removal")
}

// Group creates and returns a new route group from the router.
func (dr *Router) Group(prefix string, m ...fiber.Handler) *Group {
	var wrapped []middlewareEntry
	for _, mw := range m {
		wrapped = append(wrapped, wrapMiddleware(mw))
	}
	return &Group{
		prefix:      prefix,
		middlewares: wrapped,
		router:      dr,
	}
}

// ClearRoutes clears all dynamic routes.
func (dr *Router) ClearRoutes() {
	dr.routes = sync.Map{}
	log.Info().Msg("Cleared all dynamic routes")
}
