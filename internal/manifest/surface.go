package manifest

import (
	"fmt"
	"sort"
	"strings"
)

// Impl says who actually runs an operation. It exists because generated CRUD
// has no guest side at all (D16.4): the host serves it straight from the data
// layer, so an app whose collections are all CRUD needs no wasm module to be
// useful. Anything dispatching a tool or a route has to know which it is
// holding, and a nil function name would be a worse way to say it.
type Impl uint8

const (
	// ImplGuest calls an exported guest function.
	ImplGuest Impl = iota
	// ImplGeneratedCRUD runs host-side against one collection.
	ImplGeneratedCRUD
)

func (i Impl) String() string {
	if i == ImplGeneratedCRUD {
		return "generated_crud"
	}
	return "guest"
}

// Op is one generated CRUD verb.
type Op string

const (
	OpList   Op = "list"
	OpGet    Op = "get"
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

// Tool is one entry in tools/list, after generation and overrides.
//
// It carries no visibility of its own. Which tools a given actor sees is the
// union over installs in THAT actor's scope, resolved per connection against
// grants (D2.1), and nothing here participates in that decision.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any

	Impl Impl
	// Function is set when Impl is ImplGuest.
	Function string
	// Collection and Op are set when Impl is ImplGeneratedCRUD.
	Collection string
	Op         Op
}

// Route is one mounted HTTP route, after generation and overrides.
type Route struct {
	Method string
	// Path is relative to the install's mount point. The host owns the prefix.
	Path string

	Impl       Impl
	Function   string
	Collection string
	Op         Op
}

// DeriveVersion identifies THIS deriver, and it is recorded next to every
// surface hash a build is promoted with.
//
// Without it, two hashes that differ are ambiguous between "the app changed"
// and "we changed", and a promotion reviewer cannot tell those apart ... which
// is exactly the question the hash exists to answer. Cheap now, unanswerable
// later, because a historical row cannot be re-derived once the deriver has
// moved on.
//
// **Bump it whenever Derive's output changes for the same input**: a new field
// on Tool or Route, a different sort key, another generated operation, a
// changed CRUD schema. TestDerivedSurfaceIsGolden fails when you do, and its
// message says to come here.
//
//	1 ... first version: generated CRUD, overrides, hidden, tool tier.
const DeriveVersion = 1

// Surface is everything derived from a manifest: what this app exposes, before
// anybody asks who is connecting.
//
// # Content-addressing a Surface: use encoding/json
//
// Derive is deterministic, and for a reason rather than by luck: it never
// iterates a map, its two index maps are lookup-only, and both sorts key on
// values Validate has already made unique, so sort instability cannot bite.
// Verified over 40 CRUD collections, 40 hand-written tools with multi-key input
// schemas, 40 capabilities declared in reverse and 80 index declarations ...
// 83 KB, byte-identical across 50 derivations.
//
// **But the bytes are deterministic THROUGH encoding/json**, which sorts map
// keys. Tool.InputSchema is a map[string]any, so a registry that hashes a
// Surface with gob, or with fmt's %v, gets Go's randomized map iteration order
// back and the guarantee evaporates ... intermittently, which is the worst way
// for a content address to be wrong. Hash the JSON.
type Surface struct {
	App          string
	Kind         Kind
	Version      int
	Tools        []Tool
	Routes       []Route
	Collections  []Collection
	Capabilities []string
	// Functions the manifest promised. The registry checks these against the
	// compiled module's exports at install.
	Functions []string
}

// crudOps is the generated set, in a fixed order so a derived surface is stable
// across runs. Each entry is the tool suffix, the HTTP method, and whether the
// route addresses one document.
var crudOps = []struct {
	op     Op
	tool   string
	method string
	byID   bool
}{
	{OpList, "list", "GET", false},
	{OpGet, "get", "GET", true},
	{OpCreate, "create", "POST", false},
	{OpUpdate, "update", "PATCH", true},
	{OpDelete, "delete", "DELETE", true},
}

// Derive turns a validated manifest into the surface the host mounts.
//
// Order of resolution matters and is the one interesting thing here: generated
// operations are laid down first, then the manifest's own tools and routes
// replace any they collide with. That is what makes "the manifest can still
// rename, reshape, or hide any of them at the tool boundary" true (D16.4)
// rather than aspirational, and it means an app overrides `entries.create` by
// declaring it, not by turning CRUD off wholesale and rewriting the other four.
//
// Derive assumes Validate has passed. It is not a second validator; giving it
// one would create two places that decide whether a manifest is legal.
func (m *Manifest) Derive() Surface {
	s := Surface{
		App:          m.Name,
		Kind:         m.Kind,
		Version:      m.Version,
		Collections:  m.Storage.Collections,
		Capabilities: m.CapabilityNames(),
	}
	for _, f := range m.Functions {
		s.Functions = append(s.Functions, f.Name)
	}

	toolIdx := map[string]int{}
	routeIdx := map[string]int{}

	// A tool tier app generates nothing: no storage means no CRUD, and the
	// host skips route mounting and subscriptions for it entirely (D10.2).
	if m.Kind == KindApp {
		for _, c := range m.Storage.Collections {
			if !c.CRUD {
				continue
			}
			for _, g := range crudOps {
				name := c.Name + "." + g.tool
				toolIdx[name] = len(s.Tools)
				s.Tools = append(s.Tools, Tool{
					Name:        name,
					Description: crudDescription(c.Name, g.op),
					InputSchema: crudSchema(g.op),
					Impl:        ImplGeneratedCRUD,
					Collection:  c.Name,
					Op:          g.op,
				})

				path := "/" + c.Name
				if g.byID {
					path += "/{id}"
				}
				key := g.method + " " + path
				routeIdx[key] = len(s.Routes)
				s.Routes = append(s.Routes, Route{
					Method: g.method, Path: path,
					Impl: ImplGeneratedCRUD, Collection: c.Name, Op: g.op,
				})
			}
		}
	}

	for _, t := range m.Tools {
		if t.Hidden {
			// Hidden removes it from the surface entirely, including a
			// generated one it shadows. The function stays callable as a
			// workflow step; this is ergonomics, not a security boundary,
			// because the boundary is the grant.
			if i, ok := toolIdx[t.Name]; ok {
				s.Tools[i].Name = ""
			}
			continue
		}
		tool := Tool{
			Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
			Impl: ImplGuest, Function: t.Function,
		}
		if i, ok := toolIdx[t.Name]; ok {
			s.Tools[i] = tool
			continue
		}
		toolIdx[t.Name] = len(s.Tools)
		s.Tools = append(s.Tools, tool)
	}

	for _, r := range m.Routes {
		key := r.Method + " " + r.Path
		if r.Hidden {
			if i, ok := routeIdx[key]; ok {
				s.Routes[i].Method = ""
			}
			continue
		}
		route := Route{Method: r.Method, Path: r.Path, Impl: ImplGuest, Function: r.Function}
		if i, ok := routeIdx[key]; ok {
			s.Routes[i] = route
			continue
		}
		routeIdx[key] = len(s.Routes)
		s.Routes = append(s.Routes, route)
	}

	s.Tools = compactTools(s.Tools)
	s.Routes = compactRoutes(s.Routes)
	return s
}

// compactTools drops hidden entries and sorts, so two runs over the same
// manifest produce byte-identical surfaces. The registry content-addresses what
// it installs, and map iteration order would otherwise make that meaningless.
func compactTools(in []Tool) []Tool {
	out := in[:0]
	for _, t := range in {
		if t.Name != "" {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func compactRoutes(in []Route) []Route {
	out := in[:0]
	for _, r := range in {
		if r.Method != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// MountPath is where an install's routes hang. The host owns this prefix; an
// app that could choose its own could collide with another install (D2.3).
func MountPath(app string) string { return "/apps/" + app }

// FullPath is the absolute path for a route on a given app.
//
// **It is concatenation and it is not a boundary.** Handed a Route whose Path
// climbs out (`/../other`), it will happily produce one, because a helper that
// silently rewrote a route into a different one would be worse than a helper
// that does what it says. Validate is the boundary: it refuses `..` and every
// pattern ServeMux cannot mount, so no such Route reaches here from a validated
// manifest. TestMountPathIsHostOwned asserts both halves ... that Validate
// refuses them, and that every path it ACCEPTS still resolves inside the prefix
// after path.Clean.
func FullPath(app string, r Route) string {
	return MountPath(app) + r.Path
}

// QualifiedToolName is how a tool appears in tools/list. Prefixing by app is
// what lets tools/call route by prefix without a lookup table, and what keeps
// two installs from both offering `list`.
func QualifiedToolName(app, tool string) string { return app + "." + tool }

// SplitToolName reverses QualifiedToolName. A tool name may itself contain one
// dot (`drafts.list`), so this splits on the FIRST dot only.
func SplitToolName(qualified string) (app, tool string, ok bool) {
	app, tool, ok = strings.Cut(qualified, ".")
	if !ok || app == "" || tool == "" {
		return "", "", false
	}
	return app, tool, true
}

func crudDescription(collection string, op Op) string {
	switch op {
	case OpList:
		return fmt.Sprintf("List %s visible to you.", collection)
	case OpGet:
		return fmt.Sprintf("Fetch one %s document by id.", collection)
	case OpCreate:
		return fmt.Sprintf("Create a %s document.", collection)
	case OpUpdate:
		return fmt.Sprintf("Update a %s document you may write.", collection)
	case OpDelete:
		return fmt.Sprintf("Delete a %s document you own.", collection)
	default:
		return ""
	}
}

// crudSchema is the input contract for a generated operation.
//
// Deliberately minimal. Collections hold JSON documents with no declared shape,
// so the host cannot describe `doc` beyond "an object" without inventing a
// schema language the manifest does not have. An app that wants a tighter
// contract overrides the tool and says so.
func crudSchema(op Op) map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	id := map[string]any{"type": "string", "description": "Document id."}
	doc := map[string]any{"type": "object", "description": "The document body."}

	switch op {
	case OpList:
		return obj(map[string]any{
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
			"cursor": map[string]any{"type": "string", "description": "Opaque page cursor."},
		})
	case OpGet, OpDelete:
		return obj(map[string]any{"id": id}, "id")
	case OpCreate:
		return obj(map[string]any{"doc": doc}, "doc")
	case OpUpdate:
		return obj(map[string]any{"id": id, "doc": doc}, "id", "doc")
	default:
		return nil
	}
}
