package api

import (
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var releaseBaseline = flag.String("openapi-baseline", "", "OpenAPI JSON from the latest published version tag")

func TestOpenAPIReleaseBaseline(t *testing.T) {
	baseline := []byte(*releaseBaseline)
	if len(baseline) == 0 {
		baseline = committed(t)
	}
	var base, next map[string]any
	if err := json.Unmarshal(baseline, &base); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(committed(t), &next); err != nil {
		t.Fatal(err)
	}
	if changes := breaking(base, next); len(changes) != 0 {
		t.Fatalf("published contract changed: %v", changes)
	}
	if len(object(base, "paths")) == 0 {
		t.Fatal("baseline has no API operations")
	}
	t.Logf("OPENAPI-COMPAT-BASELINE paths=%d published=%v", len(object(base, "paths")), *releaseBaseline != "")
}

// breaking lists every change from base to next that an existing client could
// notice: a removed operation, response status or schema, a removed property,
// a removed enum value, a property that became required in a request body, or
// a new required property. Additions that are optional pass (P-11: from the
// first published version, /api/v1 changes are additive only).
func breaking(base, next map[string]any) []string {
	var out []string
	bp, np := object(base, "paths"), object(next, "paths")
	for path := range bp {
		for method, op := range object(bp, path) {
			nop, ok := object(np, path)[method]
			if !ok {
				out = append(out, fmt.Sprintf("operation removed: %s %s", strings.ToUpper(method), path))
				continue
			}
			for status, response := range object(op.(map[string]any), "responses") {
				nextResponse, ok := object(nop.(map[string]any), "responses")[status]
				if !ok {
					out = append(out, fmt.Sprintf("response removed: %s %s %s", strings.ToUpper(method), path, status))
					continue
				}
				out = append(out, contentChanges(path+" "+method+" "+status, response.(map[string]any), nextResponse.(map[string]any), false)...)
			}
			out = append(out, contentChanges(path+" "+method+" request", object(op.(map[string]any), "requestBody"), object(nop.(map[string]any), "requestBody"), true)...)
		}
	}
	bs := object(object(base, "components"), "schemas")
	ns := object(object(next, "components"), "schemas")
	requests := requestSchemas(next)
	for name, s := range bs {
		n, ok := ns[name]
		if !ok {
			out = append(out, "schema removed: "+name)
			continue
		}
		out = append(out, schemaChanges(name, s.(map[string]any), n.(map[string]any), requests[name])...)
	}
	slices.Sort(out)
	return out
}

func contentChanges(path string, base, next map[string]any, request bool) []string {
	var out []string
	for media, representation := range object(base, "content") {
		n, exists := object(next, "content")[media]
		if !exists {
			out = append(out, "representation removed: "+path+" "+media)
			continue
		}
		out = append(out, schemaChanges(path+" "+media, object(representation.(map[string]any), "schema"), object(n.(map[string]any), "schema"), request)...)
	}
	return out
}

func schemaChanges(path string, base, next map[string]any, request bool) []string {
	var out []string
	for _, key := range []string{"type", "format", "$ref"} {
		if !reflect.DeepEqual(base[key], next[key]) {
			out = append(out, fmt.Sprintf("schema %s changed: %s", key, path))
		}
	}
	for _, v := range strs(base["enum"]) {
		if values := strs(next["enum"]); len(values) > 0 && !slices.Contains(values, v) {
			out = append(out, fmt.Sprintf("enum value removed: %s %s", path, v))
		}
	}
	for name, property := range object(base, "properties") {
		n, exists := object(next, "properties")[name]
		if !exists {
			out = append(out, "property removed: "+path+"."+name)
			continue
		}
		out = append(out, schemaChanges(path+"."+name, property.(map[string]any), n.(map[string]any), request)...)
	}
	if request {
		for _, name := range strs(next["required"]) {
			if !slices.Contains(strs(base["required"]), name) {
				out = append(out, "request property now required: "+path+"."+name)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := base[key].(map[string]any); ok {
			if n, ok := next[key].(map[string]any); ok {
				out = append(out, schemaChanges(path+"."+key, child, n, request)...)
			} else {
				out = append(out, "schema removed: "+path+"."+key)
			}
		}
	}
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		children, _ := base[key].([]any)
		nextChildren, _ := next[key].([]any)
		if len(children) != len(nextChildren) {
			out = append(out, "schema alternatives changed: "+path+"."+key)
			continue
		}
		for i, child := range children {
			out = append(out, schemaChanges(fmt.Sprintf("%s.%s[%d]", path, key, i), child.(map[string]any), nextChildren[i].(map[string]any), request)...)
		}
	}
	return out
}

// requestSchemas names the component schemas used as request bodies.
func requestSchemas(doc map[string]any) map[string]bool {
	names := map[string]bool{}
	components := object(object(doc, "components"), "schemas")
	var visit func(map[string]any)
	visit = func(schema map[string]any) {
		if ref, ok := schema["$ref"].(string); ok {
			name := strings.TrimPrefix(ref, "#/components/schemas/")
			if !names[name] {
				names[name] = true
				visit(object(components, name))
			}
		}
		for _, value := range schema {
			switch value := value.(type) {
			case map[string]any:
				visit(value)
			case []any:
				for _, child := range value {
					if child, ok := child.(map[string]any); ok {
						visit(child)
					}
				}
			}
		}
	}
	for _, item := range object(doc, "paths") {
		for _, op := range item.(map[string]any) {
			for _, media := range object(object(op.(map[string]any), "requestBody"), "content") {
				visit(object(media.(map[string]any), "schema"))
			}
		}
	}
	return names
}

func object(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func strs(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, s := range list {
		if str, ok := s.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// TestOpenAPIBackwardCompatible proves the release check against baseline
// fixtures derived from the committed document: unchanged and optional
// additions pass, removals and new requirements fail.
func TestOpenAPIBackwardCompatible(t *testing.T) {
	load := func() map[string]any {
		var m map[string]any
		if err := json.Unmarshal(committed(t), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	schema := func(doc map[string]any, name string) map[string]any {
		return object(object(object(doc, "components"), "schemas"), name)
	}
	if got := breaking(load(), load()); len(got) != 0 {
		t.Fatalf("the document against itself: %v", got)
	}

	passes := map[string]func(map[string]any){
		"optional response field added": func(d map[string]any) {
			object(schema(d, "User"), "properties")["displayName"] = map[string]any{"type": "string"}
		},
		"optional request field added": func(d map[string]any) {
			object(schema(d, "Credentials"), "properties")["remember"] = map[string]any{"type": "boolean"}
		},
		"operation added": func(d map[string]any) {
			object(d, "paths")["/api/v1/extra"] = map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{}}}}
		},
		"enum value added": func(d map[string]any) {
			state := object(object(schema(d, "Status"), "properties"), "state")
			state["enum"] = append(state["enum"].([]any), "paused")
		},
	}
	for name, change := range passes {
		next := load()
		change(next)
		if got := breaking(load(), next); len(got) != 0 {
			t.Errorf("%s: reported breaking %v, want compatible", name, got)
		}
	}

	fails := map[string]func(map[string]any){
		"wire response reference changed": func(d map[string]any) {
			response := object(object(object(object(d, "paths"), "/api/v1/setup"), "post"), "responses")
			content := object(object(response, "201"), "content")
			object(object(content, "application/json"), "schema")["$ref"] = "#/components/schemas/Status"
		},
		"published id type changed": func(d map[string]any) {
			object(object(schema(d, "User"), "properties"), "id")["type"] = "string"
		},
		"published id format changed": func(d map[string]any) {
			object(object(schema(d, "User"), "properties"), "id")["format"] = "int32"
		},
		"nested response reference changed": func(d map[string]any) {
			object(object(schema(d, "SetupCreated"), "properties"), "user")["$ref"] = "#/components/schemas/Status"
		},
		"operation removed": func(d map[string]any) {
			delete(object(d, "paths"), "/api/v1/system/status")
		},
		"method removed": func(d map[string]any) {
			delete(object(object(d, "paths"), "/api/v1/setup"), "post")
		},
		"response removed": func(d map[string]any) {
			delete(object(object(object(object(d, "paths"), "/api/v1/setup"), "post"), "responses"), "409")
		},
		"response field removed": func(d map[string]any) {
			delete(object(schema(d, "User"), "properties"), "revision")
		},
		"schema removed": func(d map[string]any) {
			delete(object(object(d, "components"), "schemas"), "Status")
		},
		"enum value removed": func(d map[string]any) {
			role := object(object(schema(d, "User"), "properties"), "role")
			role["enum"] = []any{"admin", "member"}
		},
		"new required request field": func(d map[string]any) {
			c := schema(d, "Credentials")
			object(c, "properties")["otp"] = map[string]any{"type": "string"}
			c["required"] = append(c["required"].([]any), "otp")
		},
	}
	for name, change := range fails {
		next := load()
		change(next)
		if got := breaking(load(), next); len(got) == 0 {
			t.Errorf("%s: not reported as breaking", name)
		} else if name == "published id type changed" && !slices.Contains(got, "schema type changed: User.id") {
			t.Errorf("%s: missing type diagnostic: %v", name, got)
		}
	}
	t.Logf("OPENAPI-COMPAT passes=%d fails=%d", len(passes), len(fails))
}
