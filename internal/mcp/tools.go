// Package mcp serves the platform's tool surface.
//
// # The one property this package exists to hold
//
// **Everything tools/list shows, tools/call accepts. Everything tools/call
// accepts, tools/list shows.**
//
// The dangerous half is a tool that appears and then refuses. An AI reads a
// tool list as a menu of things it may do; a tool that is visible and denied
// teaches it that denials are noise to retry through, and an AI that has
// learned to treat denials as noise is one that will keep pushing on the
// denials that matter. Listing something you cannot call is worse than not
// listing it.
//
// The way that guarantee is kept is not care, it is arithmetic: **both paths
// ask the same predicate the same question.** ListTools and CallTool both call
// Guard.ToolReason with the same install and the same tool name. There is no
// second filter, no visibility flag, no "hidden" that means "denied". Two
// predicates that agree today are two predicates, and the failure mode of two
// predicates is exactly the one above.
//
// Manifest-level `hidden` is not an exception. It removes a tool from the
// surface before either path sees it, so it is invisible AND uncallable
// together ... which is the same guarantee, not a violation of it.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// Tool is one entry in tools/list, as an MCP client sees it.
type Tool struct {
	// Name is qualified by app, so tools/call can route by prefix and two
	// installs cannot both offer `list`.
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// Install is one installed app in some actor's scope, with its derived surface.
type Install struct {
	ID      uuid.UUID
	App     string
	Surface manifest.Surface
}

// Installs yields the ACTIVE installs an actor could be offered tools from.
//
// "Could be offered" rather than "may call": this is the candidate set, and
// every candidate still goes through the predicate. A source that pre-filtered
// by permission would be a second enforcement point, and the first place the
// two would disagree is the bug this package exists to prevent.
type Installs interface {
	ActiveInstalls(ctx context.Context, cred identity.Credential) ([]Install, error)
}

// Guard decides whether an actor may call one tool of one install.
//
// One method, used by both paths, deliberately. An interface with a cheap
// "can see" and an expensive "can call" would be an invitation to use the cheap
// one for listing, and the two would drift.
//
// It also carries the audit obligation: an access allowed only by an override
// writes an audit row inside this call (D18.2), which is why listing audits
// too. Listing a tool you can only see through an override IS an override
// access, and making it free would put the obligation back on one call site.
type Guard interface {
	ToolReason(ctx context.Context, cred identity.Credential, installID uuid.UUID, tool string) (Allowed, error)
}

// Allowed is the guard's verdict, reduced to what this package needs.
type Allowed bool

// Dispatcher runs a tool once the guard has said yes.
//
// It never decides permission. Handing it a credential and letting it check
// would be invariant 11's mistake: a check that takes as an argument the fact
// it is deciding about.
type Dispatcher interface {
	// CallGuest invokes an exported guest function.
	CallGuest(ctx context.Context, in GuestCall) (Result, error)
	// CallCRUD runs a generated operation host-side, with no guest involved.
	CallCRUD(ctx context.Context, in CRUDCall) (Result, error)
}

// GuestCall is one invocation of a guest function.
type GuestCall struct {
	Install  Install
	Cred     identity.Credential
	Function string
	Input    json.RawMessage
}

// CRUDCall is one generated operation.
type CRUDCall struct {
	Install    Install
	Cred       identity.Credential
	Collection string
	Op         manifest.Op
	Input      json.RawMessage
}

// Result is what a tool produced.
type Result struct {
	Output json.RawMessage
	// Trust rides out with the result, because an MCP client is frequently an
	// AI and this is the value that decides whether the content may reach
	// instruction position (invariant 9).
	Trust trust.Level
	// TaintedBy names what first made the invocation untrusted, so "why is this
	// untrusted" is answerable without reading a log.
	TaintedBy string
}

// Errors a caller can classify without matching on prose.
var (
	// ErrUnknownTool is a name no install in the actor's scope offers. It is
	// deliberately the same answer as "you may not call that": distinguishing
	// them tells an unauthorized caller which tools exist.
	ErrUnknownTool = errors.New("mcp: no such tool")

	// ErrMalformedName is a tool name that is not app-qualified.
	ErrMalformedName = errors.New("mcp: tool name is not qualified")
)

// Server answers tools/list and tools/call for one platform.
type Server struct {
	installs   Installs
	guard      Guard
	dispatcher Dispatcher
}

// New builds a server. All three collaborators are required: a nil guard would
// be a server that lists everything, which is the failure this package is about.
func New(installs Installs, guard Guard, dispatcher Dispatcher) (*Server, error) {
	if installs == nil || guard == nil || dispatcher == nil {
		return nil, errors.New("mcp: installs, guard and dispatcher are all required")
	}
	return &Server{installs: installs, guard: guard, dispatcher: dispatcher}, nil
}

// ListTools is the union over installs in the connecting actor's scope,
// filtered by the same predicate tools/call uses (D2.1).
//
// Sorted, so a client diffing two listings sees real changes rather than map
// order.
func (s *Server) ListTools(ctx context.Context, cred identity.Credential) ([]Tool, error) {
	if err := cred.Validate(); err != nil {
		return nil, err
	}
	installs, err := s.installs.ActiveInstalls(ctx, cred)
	if err != nil {
		return nil, err
	}

	var out []Tool
	for _, inst := range installs {
		for _, tool := range inst.Surface.Tools {
			ok, err := s.guard.ToolReason(ctx, cred, inst.ID, tool.Name)
			if err != nil {
				return nil, fmt.Errorf("mcp: authorize %s.%s: %w", inst.App, tool.Name, err)
			}
			if !ok {
				continue
			}
			out = append(out, Tool{
				Name:        manifest.QualifiedToolName(inst.App, tool.Name),
				Description: tool.Description,
				InputSchema: tool.InputSchema,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CallTool routes a qualified name into a guest or a generated operation.
//
// The lookup and the permission check are the same two steps ListTools takes,
// in the same order, against the same predicate. That is what makes the two
// agree, and it is why this does not consult a cached listing: a listing is a
// snapshot, and a grant revoked between listing and calling has to bite.
func (s *Server) CallTool(
	ctx context.Context, cred identity.Credential, name string, input json.RawMessage,
) (Result, error) {
	if err := cred.Validate(); err != nil {
		return Result{}, err
	}
	app, tool, ok := manifest.SplitToolName(name)
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrMalformedName, name)
	}

	installs, err := s.installs.ActiveInstalls(ctx, cred)
	if err != nil {
		return Result{}, err
	}

	for _, inst := range installs {
		if inst.App != app {
			continue
		}
		for _, t := range inst.Surface.Tools {
			if t.Name != tool {
				continue
			}
			allowed, err := s.guard.ToolReason(ctx, cred, inst.ID, t.Name)
			if err != nil {
				return Result{}, fmt.Errorf("mcp: authorize %s: %w", name, err)
			}
			if !allowed {
				// Same error as "no such tool", on purpose. Telling an
				// unauthorized caller that a tool exists is an existence
				// oracle, and the tool list they can see already tells them
				// everything they are allowed to know.
				return Result{}, fmt.Errorf("%w: %q", ErrUnknownTool, name)
			}

			switch t.Impl {
			case manifest.ImplGeneratedCRUD:
				return s.dispatcher.CallCRUD(ctx, CRUDCall{
					Install: inst, Cred: cred,
					Collection: t.Collection, Op: t.Op, Input: input,
				})
			default:
				return s.dispatcher.CallGuest(ctx, GuestCall{
					Install: inst, Cred: cred, Function: t.Function, Input: input,
				})
			}
		}
	}
	return Result{}, fmt.Errorf("%w: %q", ErrUnknownTool, name)
}
