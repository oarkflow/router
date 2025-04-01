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

type ErrorResponse struct {
	Timestamp time.Time `json:"timestamp"`
	Error     string    `json:"error"`
}

func customErrorHandler(c *fiber.Ctx, err error) error {
	errResp := ErrorResponse{
		Timestamp: time.Now(),
		Error:     err.Error(),
	}
	log.Error().Msg(err.Error())
	return c.Status(fiber.StatusInternalServerError).JSON(errResp)
}

func matchRoute(pattern, path string) (bool, map[string]string) {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")
	if len(patternParts) != len(pathParts) {
		return false, nil
	}
	params := map[string]string{}
	for i := 0; i < len(patternParts); i++ {
		pp := patternParts[i]
		ap := pathParts[i]
		if strings.HasPrefix(pp, ":") {
			key := pp[1:]
			params[key] = ap
		} else if pp != ap {
			return false, nil
		}
	}
	return true, params
}

type Route struct {
	Method      string
	Path        string
	Handler     fiber.Handler
	Middlewares []middlewareEntry
	Renderer    fiber.Views
}

func (dr *Route) Serve(c *fiber.Ctx, globalMWs []middlewareEntry) error {
	chain := make([]fiber.Handler, 0, len(globalMWs)+len(dr.Middlewares)+1)
	for _, m := range globalMWs {
		chain = append(chain, m.handler)
	}
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

type Router struct {
	app               *fiber.App
	lock              sync.RWMutex
	routes            map[string]*methodRoutes
	staticRoutes      []Static
	GlobalMiddlewares []middlewareEntry
	NotFoundHandler   fiber.Handler
	staticCache       map[string]staticCacheEntry
	staticCacheLock   sync.RWMutex
}

func New(app *fiber.App) *Router {
	dr := &Router{
		app:               app,
		routes:            make(map[string]*methodRoutes),
		staticCache:       make(map[string]staticCacheEntry),
		GlobalMiddlewares: []middlewareEntry{},
	}

	app.Use(func(c *fiber.Ctx) error {
		err := c.Next()
		if err != nil {
			return customErrorHandler(c, err)
		}
		return nil
	})
	app.All("/*", dr.dispatch)
	return dr
}

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
		return fmt.Errorf("middleware[%d] error: %w", idx, err)
	}
	return nil
}

func (dr *Router) Use(mw ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	for _, m := range mw {
		dr.GlobalMiddlewares = append(dr.GlobalMiddlewares, wrapMiddleware(m))
	}
	log.Info().Int("count", len(mw)).Msg("Added global middleware")
}

func (dr *Router) dispatch(c *fiber.Ctx) error {
	dr.lock.RLock()
	defer dr.lock.RUnlock()
	method := c.Method()
	path := c.Path()

	if mr, ok := dr.routes[method]; ok {
		if route, exists := mr.exact[path]; exists {
			return route.Serve(c, dr.GlobalMiddlewares)
		}
		for _, route := range mr.params {
			if matched, params := matchRoute(route.Path, path); matched {
				c.Locals("params", params)
				return route.Serve(c, dr.GlobalMiddlewares)
			}
		}
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

func (dr *Router) AddRoute(method, path string, handler fiber.Handler, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if dr.routes[method] == nil {
		dr.routes[method] = &methodRoutes{
			exact:  make(map[string]*Route),
			params: []*Route{},
		}
	}
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
		dr.routes[method].params = append(dr.routes[method].params, route)
	} else {
		dr.routes[method].exact[path] = route
	}
	log.Info().Str("method", method).Str("path", path).Msg("Added dynamic route")
}

func (dr *Router) UpdateRoute(method, path string, newHandler fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if mr, ok := dr.routes[method]; ok {
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

func (dr *Router) RenameRoute(method, oldPath, newPath string) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if mr, ok := dr.routes[method]; ok {
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

func (dr *Router) AddMiddleware(method, path string, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if mr, ok := dr.routes[method]; ok {
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

func (dr *Router) RemoveMiddleware(method, path string, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if mr, ok := dr.routes[method]; ok {
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

func (dr *Router) SetRenderer(method, path string, renderer fiber.Views) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if mr, ok := dr.routes[method]; ok {
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

func (dr *Router) Static(prefix, directory string, cfg ...StaticConfig) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
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

func (dr *Router) RemoveRoute(method, path string) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if mr, ok := dr.routes[method]; ok {
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

func (dr *Router) SetNotFoundHandler(handler fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	dr.NotFoundHandler = handler
	log.Info().Msg("Set custom NotFoundHandler")
}

func (dr *Router) ListRoutes() []string {
	dr.lock.RLock()
	defer dr.lock.RUnlock()
	var routesList []string
	for method, mr := range dr.routes {
		for path := range mr.exact {
			routesList = append(routesList, method+" "+path)
		}
		for _, route := range mr.params {
			routesList = append(routesList, method+" "+route.Path)
		}
	}
	return routesList
}

func (dr *Router) InvalidateStaticCache(file string) {
	dr.staticCacheLock.Lock()
	defer dr.staticCacheLock.Unlock()
	delete(dr.staticCache, file)
	log.Info().Str("file", file).Msg("Invalidated static cache")
}

func (dr *Router) Shutdown() error {
	log.Info().Msg("Initiating graceful shutdown")
	return dr.app.Shutdown()
}

type GroupRoute struct {
	method        string
	relPath       string
	handler       fiber.Handler
	routeMWs      []middlewareEntry
	effectivePath string
}

type Group struct {
	prefix      string
	middlewares []middlewareEntry
	routes      []*GroupRoute
	router      *Router
}

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

func (g *Group) Get(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("GET", relPath, handler, m...)
}
func (g *Group) Post(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("POST", relPath, handler, m...)
}
func (g *Group) Put(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("PUT", relPath, handler, m...)
}
func (g *Group) Delete(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("DELETE", relPath, handler, m...)
}
func (g *Group) Patch(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("PATCH", relPath, handler, m...)
}
func (g *Group) Options(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("OPTIONS", relPath, handler, m...)
}
func (g *Group) Head(relPath string, handler fiber.Handler, m ...fiber.Handler) {
	g.AddRoute("HEAD", relPath, handler, m...)
}

func (g *Group) Static(prefix, directory string, cfg ...StaticConfig) {
	fullPrefix := g.prefix + prefix
	g.router.Static(fullPrefix, directory, cfg...)
}

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

func (g *Group) AddMiddleware(mw ...fiber.Handler) {
	current := make([]fiber.Handler, 0, len(g.middlewares))
	for _, m := range g.middlewares {
		current = append(current, m.handler)
	}
	current = append(current, mw...)
	g.UpdateMiddlewares(current)
}

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
