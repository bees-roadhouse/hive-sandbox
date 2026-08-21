// Package manifest is the app declaration and everything derived from it.
//
// The manifest is the single source of truth for three surfaces plus
// capabilities (D2.4), which makes it the most load-bearing artifact in the
// design: every app forever declares itself through it. So this package is
// deliberately pure. It parses, validates and derives; it opens no connections,
// mounts no routes and runs no guests. Everything with I/O consumes what comes
// out of here.
//
// # What a manifest may NOT declare
//
// It says what an app HAS, never who may reach it. There is no field for
// visibility, no field for allowed actors, no field for an access rule. Those
// are grants, resolved per request against the connecting actor, and a manifest
// that could express them would be a second enforcement point in a file the app
// author controls (invariant 1).
//
// Capabilities are the apparent exception and are not one: declaring a
// capability is a request, not a grant. The manifest says which host modules the
// guest links; whether the install may actually use them is a grant, and the
// host refuses to link an undeclared one at load time either way.
package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Kind separates the two tiers. A tool is a verb: JSON in, JSON out, owns no
// data (D10.1). An app owns data. The distinction is enforced here rather than
// trusted, because the whole point of the tool tier is that the host can skip
// schema provisioning, route mounting and subscriptions for it.
type Kind string

const (
	KindApp  Kind = "app"
	KindTool Kind = "tool"
)

// Manifest is what an app author writes.
type Manifest struct {
	Kind    Kind   `json:"kind" yaml:"kind"`
	Name    string `json:"name" yaml:"name"`
	Version int    `json:"version" yaml:"version"`

	Storage       Storage        `json:"storage,omitzero" yaml:"storage,omitempty"`
	Functions     []Function     `json:"functions,omitempty" yaml:"functions,omitempty"`
	Tools         []ToolDef      `json:"tools,omitempty" yaml:"tools,omitempty"`
	Routes        []RouteDef     `json:"routes,omitempty" yaml:"routes,omitempty"`
	Subscriptions []Subscription `json:"subscriptions,omitempty" yaml:"subscriptions,omitempty"`
	Capabilities  []string       `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
}

// Storage is the collections an app declares. The host owns all DDL (D3.3): an
// app names collections and indexes, and never brings an engine or runs SQL.
type Storage struct {
	Collections []Collection `json:"collections,omitempty" yaml:"collections,omitempty"`
}

// Collection is one JSON document store inside the app's schema.
type Collection struct {
	Name string `json:"name" yaml:"name"`

	// CRUD asks the host to generate list, get, create, update and delete
	// (D16.4). Generated operations run entirely host-side against the data
	// layer, actor-scoped and grant-filtered, with no guest code involved.
	//
	// That is the ergonomic win for AI-built apps in particular: most apps are
	// mostly CRUD, and generating it removes the part an AI is most likely to
	// get subtly wrong. Hand-written functions are for collections where a
	// write does more than store ... in the standard set that is almost always
	// because it has to fan out into mentions, grants or projections.
	CRUD bool `json:"crud,omitempty" yaml:"crud,omitempty"`

	Indexes []string `json:"indexes,omitempty" yaml:"indexes,omitempty"`
}

// Function is a guest export the manifest promises exists. The host checks the
// promise against the compiled module at install rather than discovering a
// missing export on a user's first call.
type Function struct {
	Name string `json:"name" yaml:"name"`
	Doc  string `json:"doc,omitempty" yaml:"doc,omitempty"`
}

// ToolDef maps a tool name onto a function, and it is separate from the
// function on purpose (D2.5).
//
// Tool definitions are the stable surface: an app may rename, reshape, compose
// or hide its functions at this boundary without the tool surface moving under
// the AIs that call it. One-to-one generation would have coupled them, and the
// tool surface is the thing every AI on the platform reaches memory through.
type ToolDef struct {
	Name string `json:"name" yaml:"name"`
	// Function is the guest export this tool invokes. Empty on a generated
	// CRUD tool, which has no guest side at all.
	Function    string `json:"function,omitempty" yaml:"function,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// InputSchema is JSON Schema shown to an AI caller. Shaping lives here so
	// the guest can take a wider or uglier input than the tool advertises.
	InputSchema map[string]any `json:"input_schema,omitempty" yaml:"input_schema,omitempty"`
	// Hidden keeps a function callable as a workflow step and out of
	// tools/list. Not a security boundary ... grants are.
	Hidden bool `json:"hidden,omitempty" yaml:"hidden,omitempty"`
}

// RouteDef mounts one HTTP route onto a function. Symmetric with ToolDef, for
// the same reason: the URL surface should not move when a guest's internals do.
type RouteDef struct {
	Method string `json:"method" yaml:"method"`
	// Path is relative to the install's mount point, so it always starts with
	// "/" and never contains the app name. The host owns the prefix; an app
	// that could choose its own could collide with another install.
	Path     string `json:"path" yaml:"path"`
	Function string `json:"function,omitempty" yaml:"function,omitempty"`
	Hidden   bool   `json:"hidden,omitempty" yaml:"hidden,omitempty"`
}

// Subscription is an event pattern the app reacts to. Delivery is still
// grant-filtered at dispatch: an app receives only events on entities its
// install could read (D1.5), so declaring a broad pattern widens nothing.
type Subscription struct {
	Kind string `json:"kind" yaml:"kind"`
}

// Errors returned by Validate. They are values rather than strings so a
// registry can classify a bad manifest without matching on prose.
var (
	ErrKind         = errors.New("manifest: unknown kind")
	ErrName         = errors.New("manifest: invalid name")
	ErrVersion      = errors.New("manifest: version must be positive")
	ErrToolTier     = errors.New("manifest: a tool owns no data and mounts nothing")
	ErrDuplicate    = errors.New("manifest: duplicate name")
	ErrUnknownFunc  = errors.New("manifest: reference to an undeclared function")
	ErrReservedName = errors.New("manifest: name collides with a generated one")
	ErrRoute        = errors.New("manifest: invalid route")
)

// nameRE is deliberately narrow. These strings become schema names, tool names,
// URL segments and JSON keys, and the set that is safe in all four at once is
// small. Rejecting at install is cheap; discovering that a name is legal in a
// manifest and illegal in Postgres is not.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Validate checks everything decidable from the manifest alone.
//
// It does not check the module: whether the guest actually exports what the
// functions section promises needs the compiled bytes, and belongs to the
// registry at install. Everything here can be answered by reading the file, and
// answering it here means a malformed manifest never reaches the parts that
// touch Postgres.
func (m *Manifest) Validate() error {
	var errs []error

	switch m.Kind {
	case KindApp, KindTool:
	default:
		errs = append(errs, fmt.Errorf("%w: %q", ErrKind, m.Kind))
	}
	if !nameRE.MatchString(m.Name) {
		errs = append(errs, fmt.Errorf("%w: %q must match %s", ErrName, m.Name, nameRE))
	}
	if m.Version < 1 {
		errs = append(errs, fmt.Errorf("%w: %d", ErrVersion, m.Version))
	}

	// The tool tier is a contract, not a convention (D10.3). A tool that could
	// declare storage would be an app with a smaller word, and the host's
	// ability to skip provisioning for tools depends on this holding.
	if m.Kind == KindTool {
		switch {
		case len(m.Storage.Collections) > 0:
			errs = append(errs, fmt.Errorf("%w: %q declares storage", ErrToolTier, m.Name))
		case len(m.Routes) > 0:
			errs = append(errs, fmt.Errorf("%w: %q declares routes", ErrToolTier, m.Name))
		case len(m.Subscriptions) > 0:
			errs = append(errs, fmt.Errorf("%w: %q declares subscriptions", ErrToolTier, m.Name))
		case len(m.Functions) != 1:
			errs = append(errs, fmt.Errorf("%w: %q declares %d functions, a tool has exactly one",
				ErrToolTier, m.Name, len(m.Functions)))
		}
	}

	funcs := make(map[string]bool, len(m.Functions))
	for _, f := range m.Functions {
		if !nameRE.MatchString(f.Name) {
			errs = append(errs, fmt.Errorf("%w: function %q", ErrName, f.Name))
			continue
		}
		if funcs[f.Name] {
			errs = append(errs, fmt.Errorf("%w: function %q", ErrDuplicate, f.Name))
		}
		funcs[f.Name] = true
	}

	collections := make(map[string]bool, len(m.Storage.Collections))
	generated := make(map[string]bool)
	for _, c := range m.Storage.Collections {
		if !nameRE.MatchString(c.Name) {
			errs = append(errs, fmt.Errorf("%w: collection %q", ErrName, c.Name))
			continue
		}
		if collections[c.Name] {
			errs = append(errs, fmt.Errorf("%w: collection %q", ErrDuplicate, c.Name))
		}
		collections[c.Name] = true
		if c.CRUD {
			for _, op := range crudOps {
				generated[c.Name+"."+op.tool] = true
			}
		}
	}

	seenTools := make(map[string]bool, len(m.Tools))
	for _, t := range m.Tools {
		if !toolNameRE.MatchString(t.Name) {
			errs = append(errs, fmt.Errorf("%w: tool %q", ErrName, t.Name))
			continue
		}
		if seenTools[t.Name] {
			errs = append(errs, fmt.Errorf("%w: tool %q", ErrDuplicate, t.Name))
		}
		seenTools[t.Name] = true

		// A hand-written tool may deliberately replace a generated one, or hide
		// it. That is what "the manifest can still rename, reshape, or hide any
		// of them" means (D16.4). What it may not do is collide by ACCIDENT, so
		// naming a generated tool has to be unambiguous: either declare a
		// function to override it, or declare it hidden to remove it. An entry
		// that does neither is somebody who did not know the name was taken.
		if generated[t.Name] && t.Function == "" && !t.Hidden {
			errs = append(errs, fmt.Errorf(
				"%w: tool %q is generated; declare a function to override it, or hidden to remove it",
				ErrReservedName, t.Name))
		}
		if t.Function != "" && !funcs[t.Function] {
			errs = append(errs, fmt.Errorf("%w: tool %q -> %q", ErrUnknownFunc, t.Name, t.Function))
		}
	}

	for _, r := range m.Routes {
		if err := r.validate(funcs); err != nil {
			errs = append(errs, err)
		}
	}

	for _, s := range m.Subscriptions {
		if s.Kind == "" {
			errs = append(errs, fmt.Errorf("%w: subscription with no kind", ErrName))
		}
	}

	return errors.Join(errs...)
}

// toolNameRE allows one dot, because tool names are `collection.verb` and
// `app.verb` by convention and both read badly with an underscore.
var toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`)

var methods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

func (r RouteDef) validate(funcs map[string]bool) error {
	if !methods[r.Method] {
		return fmt.Errorf("%w: method %q", ErrRoute, r.Method)
	}
	if !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("%w: path %q must start with /", ErrRoute, r.Path)
	}
	if strings.Contains(r.Path, "..") {
		return fmt.Errorf("%w: path %q", ErrRoute, r.Path)
	}
	if r.Function != "" && !funcs[r.Function] {
		return fmt.Errorf("%w: route %s %s -> %q", ErrUnknownFunc, r.Method, r.Path, r.Function)
	}
	return nil
}

// CapabilityNames returns the declared capabilities, sorted and deduplicated.
// The host turns these into a wasmhost.CapabilitySet; this package does not
// import wasmhost, so that a manifest can be validated without a runtime.
func (m *Manifest) CapabilityNames() []string {
	seen := make(map[string]bool, len(m.Capabilities))
	out := make([]string, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
