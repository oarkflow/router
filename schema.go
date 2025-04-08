package router

import (
	"log"
	"sync"

	"github.com/oarkflow/json"
	v2 "github.com/oarkflow/json/jsonschema/v2"
)

type Schema struct {
	m     sync.RWMutex
	items map[string]*v2.Schema
}

var (
	compiledSchemas *Schema
	compiler        *v2.Compiler
)

func init() {
	compiler = v2.NewCompiler()
	compiledSchemas = &Schema{items: make(map[string]*v2.Schema)}
}

func AddSchema(key string, schema *v2.Schema) {
	compiledSchemas.m.Lock()
	defer compiledSchemas.m.Unlock()
	compiledSchemas.items[key] = schema
}

func CompileSchema(uri, method string, schema json.RawMessage) {
	s, err := compiler.Compile(schema)
	if err != nil {
		log.Printf("Error compiling schema for %s %s: %v", method, uri, err)
		return
	}
	key := method + ":" + uri
	AddSchema(key, s)
}
