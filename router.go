package router

import (
	"fmt"
	"log"
	"mime"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v2"

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

type Route struct {
	Method  string
	Path    string
	Handler fiber.Handler

	Middlewares []middlewareEntry
	Renderer    fiber.Views
}

func (dr *Route) Serve(c *fiber.Ctx, router *Router, globalMWs []middlewareEntry) error {

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
	if err := router.Next(c); err != nil {
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

type Static struct {
	Prefix           string `json:"prefix"`
	Directory        string `json:"directory"`
	CacheControl     string
	DirectoryListing bool
	CompressionLevel int
}

type Router struct {
	app               *fiber.App
	lock              sync.RWMutex
	routes            map[string]map[string]*Route
	staticRoutes      []Static
	GlobalMiddlewares []middlewareEntry
	NotFoundHandler   fiber.Handler
	staticCache       map[string][]byte
	staticCacheLock   sync.RWMutex
}

func New(app *fiber.App) *Router {
	dr := &Router{
		app:               app,
		routes:            make(map[string]map[string]*Route),
		staticCache:       make(map[string][]byte),
		GlobalMiddlewares: []middlewareEntry{},
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
	log.Printf("info msg=\"Added global middleware\" count=%d", len(mw))
}

func (dr *Router) dispatch(c *fiber.Ctx) error {
	dr.lock.RLock()
	defer dr.lock.RUnlock()
	method := c.Method()
	path := c.Path()
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			return route.Serve(c, dr, dr.GlobalMiddlewares)
		}
	}
	for _, sr := range dr.staticRoutes {
		if strings.HasPrefix(path, sr.Prefix) {
			relativePath := strings.TrimPrefix(path, sr.Prefix)
			filePath := filepath.Join(sr.Directory, relativePath)
			info, err := os.Stat(filePath)
			if err == nil && info.IsDir() {
				if sr.DirectoryListing {
					entries, err := os.ReadDir(filePath)
					if err != nil {
						log.Printf("error msg=\"Failed to read directory\" error=%v file=%s", err, filePath)
						return c.Status(500).SendString("Error reading directory")
					}
					var builder strings.Builder
					builder.WriteString("<html><head><meta charset=\"UTF-8\"><title>Directory listing</title></head><body>")
					builder.WriteString("<h1>Directory listing for " + c.Path() + "</h1><ul>")
					for _, entry := range entries {
						name := entry.Name()
						// Compute a URL path for the entry.
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
				}
				if sr.CacheControl != "" {
					c.Response().Header.Set("Cache-Control", sr.CacheControl)
				}
				var data []byte
				dr.staticCacheLock.RLock()
				cached, found := dr.staticCache[filePath]
				dr.staticCacheLock.RUnlock()
				if found {
					data = cached
					log.Printf("info msg=\"Static cache hit\" file=%s", filePath)
				} else {
					d, err := os.ReadFile(filePath)
					if err != nil {
						log.Printf("error msg=\"Error reading file\" file=%s error=%v", filePath, err)
						return c.Status(500).SendString("Error reading file")
					}
					data = d
					dr.staticCacheLock.Lock()
					dr.staticCache[filePath] = data
					dr.staticCacheLock.Unlock()
				}
				compData, err := compressData(c, data)
				if err != nil {
					log.Printf("error msg=\"Compression error\" file=%s error=%v", filePath, err)
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
		dr.routes[method] = make(map[string]*Route)
	}
	var mwEntries []middlewareEntry
	for _, m := range middlewares {
		mwEntries = append(mwEntries, wrapMiddleware(m))
	}
	dr.routes[method][path] = &Route{
		Method:      method,
		Path:        path,
		Handler:     handler,
		Middlewares: mwEntries,
	}
	log.Printf("info msg=\"Added dynamic route\" method=%s path=%s", method, path)
}

func (dr *Router) UpdateRoute(method, path string, newHandler fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			route.Handler = newHandler
			log.Printf("info msg=\"Updated dynamic route handler\" method=%s path=%s", method, path)
			return
		}
	}
	log.Printf("warn msg=\"Route not found for update\" method=%s path=%s", method, path)
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
			log.Printf("info msg=\"Renamed route\" oldPath=%s newPath=%s method=%s", oldPath, newPath, method)
			return
		}
	}
	log.Printf("warn msg=\"Route not found for rename\" method=%s oldPath=%s", method, oldPath)
}

func (dr *Router) AddMiddleware(method, path string, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			for _, m := range middlewares {
				route.Middlewares = append(route.Middlewares, wrapMiddleware(m))
			}
			log.Printf("info msg=\"Added middleware to route\" method=%s path=%s count=%d", method, path, len(middlewares))
			return
		}
	}
	log.Printf("warn msg=\"Route not found for adding middleware\" method=%s path=%s", method, path)
}

func (dr *Router) RemoveMiddleware(method, path string, middlewares ...fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
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
			log.Printf("info msg=\"Removed middleware from route\" method=%s path=%s", method, path)
			return
		}
	}
	log.Printf("warn msg=\"Route not found for removing middleware\" method=%s path=%s", method, path)
}

func (dr *Router) SetRenderer(method, path string, renderer fiber.Views) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if route, exists := methodRoutes[path]; exists {
			route.Renderer = renderer
			log.Printf("info msg=\"Set custom renderer for route\" method=%s path=%s", method, path)
			return
		}
	}
	log.Printf("warn msg=\"Route not found for setting renderer\" method=%s path=%s", method, path)
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
	log.Printf("info msg=\"Added static route\" prefix=%s directory=%s", prefix, directory)
}

func (dr *Router) RemoveRoute(method, path string) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	method = strings.ToUpper(method)
	if methodRoutes, ok := dr.routes[method]; ok {
		if _, exists := methodRoutes[path]; exists {
			delete(methodRoutes, path)
			log.Printf("info msg=\"Removed dynamic route\" method=%s path=%s", method, path)
			return
		}
	}
	log.Printf("warn msg=\"Route not found for removal\" method=%s path=%s", method, path)
}

func (dr *Router) SetNotFoundHandler(handler fiber.Handler) {
	dr.lock.Lock()
	defer dr.lock.Unlock()
	dr.NotFoundHandler = handler
	log.Printf("info msg=\"Custom NotFoundHandler set\"")
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

func (dr *Router) InvalidateStaticCache(file string) {
	dr.staticCacheLock.Lock()
	defer dr.staticCacheLock.Unlock()
	delete(dr.staticCache, file)
	log.Printf("info msg=\"Invalidated static cache\" file=%s", file)
}

type StaticConfig struct {
	Compress         bool
	ByteRange        bool
	CacheControl     string
	DirectoryListing bool
	CompressionLevel int
}

type GroupRoute struct {
	method  string
	relPath string
	handler fiber.Handler

	routeMWs      []middlewareEntry
	effectivePath string
}

type Group struct {
	prefix string

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
	log.Printf("info msg=\"Group prefix changed\" oldPrefix=%s newPrefix=%s", oldPrefix, newPrefix)
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
	log.Printf("info msg=\"Group middlewares updated\" groupPrefix=%s", g.prefix)
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
			log.Printf("info msg=\"Removed group route\" method=%s relPath=%s", gr.method, relPath)
			return
		}
	}
	log.Printf("warn msg=\"Group route not found for removal\" relPath=%s", relPath)
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
