package router

import (
	"github.com/gofiber/fiber/v2"
	"github.com/oarkflow/json"
)

// ValidateRequestBySchema - validates each request that has schema validation
func (dr *Router) ValidateRequestBySchema(c *fiber.Ctx) error {
	route, matched, _ := dr.MatchRoute(c.Method(), c.Path())
	if !matched {
		return Next(c)
	}
	key := route.Method + ":" + route.Path
	compiledSchemas.m.Lock()
	schema, exists := compiledSchemas.items[key]
	compiledSchemas.m.Unlock()
	if !exists {
		return Next(c)
	}
	body := c.Body()
	if len(body) == 0 {
		return Next(c)
	}
	var intermediate any
	if err := schema.UnmarshalFiberCtx(c, &intermediate); err != nil {
		return err
	}
	mergedBytes, err := json.Marshal(intermediate)
	if err != nil {
		return err
	}
	c.Request().SetBody(mergedBytes)
	return Next(c)
}
