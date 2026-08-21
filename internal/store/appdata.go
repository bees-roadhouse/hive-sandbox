package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// AppData is the host-mediated data layer guests reach through host.storage.*.
//
// Guests never see SQL, so this is where "may this actor touch this row" is
// actually decided, and it decides it exactly one way: through Guard, which
// runs access_decision(). No query here composes its own filter.
//
// One call is one transaction, which is what lets a write fan out into the
// document, its entity row and its event without a window where an event names
// something a subscriber cannot yet read (D13.2).
//
// # Not done yet, and it is not optional
//
// A document may contain blob descriptors, and D6 finding 2 makes maintaining
// blob_refs a REQUIREMENT of host.storage.*: the host walks each document it is
// handed, extracts descriptor-shaped objects, and writes the references in the
// same transaction. Without it, either blobs leak forever or a delete corrupts
// a live document, and mark-and-sweep over app JSONB is the wrong fix because
// it is O(corpus) and still misses a descriptor sitting in a workflow step's
// arguments.
//
// This does not do that yet, deliberately rather than by oversight. The only
// entry point available today is blob.AddRef, which takes a bare hash and hands
// back a reference ... so extracting a digest from a guest's JSON and calling it
// would make knowing a sha256 enough to read someone else's bytes. That is the
// bug Augie found and Melissa is replacing with LinkRef, which authorizes from
// a reference the caller already holds rather than from the digest.
//
// So: when LinkRef lands, extraction goes here, in Insert, Update and Delete,
// on the transaction those already have. Until then a document may carry a
// descriptor and nothing will hold the bytes down for it.
type AppData struct {
	store *Store
}

// NewAppData wires the data layer over a store.
func NewAppData(s *Store) *AppData { return &AppData{store: s} }

// Compile-time proof that this satisfies the ABI's expectations.
var _ wasmhost.Storage = (*AppData)(nil)

// identSafe bounds what can reach pgx.Identifier. Collection names are also
// checked against the manifest, so this is the second of two gates rather than
// the only one: a name that passes here and is not declared is still refused.
var identSafe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,58}$`)

// docRequest is the body every storage verb takes. Fields not relevant to a
// verb are ignored rather than rejected, so a guest SDK can share one struct.
//
// Nothing here is an assertion about identity, ownership or trust. Those come
// from the Request the host filled in, and a guest cannot reach them.
type docRequest struct {
	Collection string          `json:"collection"`
	ID         string          `json:"id,omitempty"`
	Ref        string          `json:"ref,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Doc        json.RawMessage `json:"doc,omitempty"`

	// Match is a JSONB containment filter: {"status":"open"} finds documents
	// whose doc contains that. Containment rather than an expression language
	// on purpose ... an expression a guest composes is both an injection
	// surface and a place for a second access policy to grow.
	Match json.RawMessage `json:"match,omitempty"`

	Limit int    `json:"limit,omitempty"`
	After string `json:"after,omitempty"`
}

// docRow is what a read returns.
type docRow struct {
	ID        uuid.UUID       `json:"id"`
	Ref       string          `json:"ref"`
	Kind      string          `json:"kind"`
	Doc       json.RawMessage `json:"doc"`
	Trust     trust.Level     `json:"trust"`
	TaintedBy string          `json:"tainted_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// installInfo is everything a call needs to know about where it is running.
type installInfo struct {
	ID     uuid.UUID
	Slug   string
	Schema string
	Owner  Owner
}

// resolveInstall looks up the install and refuses anything but an active one.
//
// The state check is here rather than only where a Caller is minted, and that
// is deliberate. D19.4 makes promoting a build a distinct human act; a staged
// install already has a row, a schema name and real tables, so an unpromoted
// build that reached a guest invocation would run. Whoever mints the install id
// onto a Caller should check too, and this is the backstop that does not depend
// on them remembering.
func resolveInstall(ctx context.Context, db DB, installID uuid.UUID) (installInfo, map[string]bool, error) {
	var (
		info        installInfo
		ownerKind   string
		state       string
		collections []string
	)
	err := db.QueryRow(ctx, `
		SELECT i.id, i.slug, i.schema_name, i.owner_kind, i.owner_id, i.state,
		       coalesce(
		           (SELECT array_agg(c->>'name')
		              FROM jsonb_array_elements(b.manifest->'storage'->'collections') AS c),
		           ARRAY[]::text[])
		  FROM installs i
		  JOIN app_builds b ON b.id = i.build_id
		 WHERE i.id = $1`, installID,
	).Scan(&info.ID, &info.Slug, &info.Schema, &ownerKind, &info.Owner.ID, &state, &collections)
	if errors.Is(err, pgx.ErrNoRows) {
		return installInfo{}, nil, wasmhost.Errorf(wasmhost.StatusNotFound, "install %s does not exist", installID)
	}
	if err != nil {
		return installInfo{}, nil, fmt.Errorf("resolve install: %w", err)
	}
	if state != "active" {
		return installInfo{}, nil, wasmhost.Errorf(wasmhost.StatusDenied,
			"install %s is %s, not active: a build nobody promoted does not serve calls (D19.4)",
			installID, state)
	}
	info.Owner.Kind = PrincipalKind(ownerKind)

	declared := make(map[string]bool, len(collections))
	for _, c := range collections {
		declared[c] = true
	}
	return info, declared, nil
}

// collectionSubject is what a grant on a whole collection is written against.
func collectionSubject(installID uuid.UUID, collection string) Subject {
	return Subject{Kind: SubjectCollection, ID: installID, Name: collection}
}

// entitySubject is what a grant on one document is written against. Documents
// are grantable individually because that is what a tag does (D13).
func entitySubject(id uuid.UUID) Subject { return Subject{Kind: SubjectEntity, ID: id} }

// denied maps the guard's refusal onto a status the guest sees.
//
// A refused READ is StatusNotFound, deliberately: telling "no such row" apart
// from "not allowed to see it" is an existence oracle, and the ABI has one
// status for both so a guest cannot probe. A refused WRITE is StatusDenied,
// because the caller already knows the collection exists ... it is declared in
// the manifest it shipped.
func denied(err error, asNotFound bool) error {
	if !errors.Is(err, ErrDenied) {
		return err
	}
	if asNotFound {
		return wasmhost.Errorf(wasmhost.StatusNotFound, "no such document")
	}
	return wasmhost.Errorf(wasmhost.StatusDenied, "denied")
}

// parse decodes the body and validates the collection against the manifest.
func parse(req wasmhost.Request, declared map[string]bool) (docRequest, error) {
	var d docRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &d); err != nil {
			return d, wasmhost.Errorf(wasmhost.StatusInvalid, "body is not an object: %v", err)
		}
	}
	if d.Collection == "" {
		return d, wasmhost.Errorf(wasmhost.StatusInvalid, "collection is required")
	}
	if !identSafe.MatchString(d.Collection) {
		return d, wasmhost.Errorf(wasmhost.StatusInvalid, "collection %q is not a valid name", d.Collection)
	}
	// Declared in the manifest, or it does not exist for this app. The host
	// owns all DDL, so an undeclared collection has no table and asking for one
	// is a manifest error rather than a missing row.
	if !declared[d.Collection] {
		return d, wasmhost.Errorf(wasmhost.StatusNotFound,
			"collection %q is not declared by this app", d.Collection)
	}
	return d, nil
}

func (d docRequest) docID() (uuid.UUID, error) {
	if d.ID == "" {
		return uuid.Nil, wasmhost.Errorf(wasmhost.StatusInvalid, "id is required")
	}
	id, err := uuid.Parse(d.ID)
	if err != nil {
		return uuid.Nil, wasmhost.Errorf(wasmhost.StatusInvalid, "id %q is not a uuid", d.ID)
	}
	return id, nil
}

func table(schema, collection string) string {
	return pgx.Identifier{schema, collection}.Sanitize()
}

// writeTrust is the trust a write lands as, and it only ever moves one way.
//
// The invocation's taint is one floor: a write made after an untrusted read is
// untrusted whatever the guest claims (invariant 12). The row's existing trust
// is the other: a trusted invocation updating an untrusted row does NOT clean it,
// because raising trust is what the sanitizer is for and nothing else may do it
// (D22.3). Both floors point the same way, so this is just trust.Weaker, which
// already exists precisely so nobody writes a second version of it.
func writeTrust(existing, incoming trust.Level) trust.Level {
	return trust.Weaker(existing, incoming)
}

// Insert stores a document.
//
// The document row, its entity row and the event announcing it are one
// transaction. That ordering rule is D13.2's: a subscriber must never receive
// an event for something it is not yet allowed to read, and the only way to
// guarantee that is to write the grant-bearing row and the event together.
func (a *AppData) Insert(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	var out wasmhost.Response
	txErr := a.store.InTx(ctx, func(tx pgx.Tx) error {
		info, declared, resolveErr := resolveInstall(ctx, tx, req.Caller.InstallID)
		if resolveErr != nil {
			return resolveErr
		}
		d, parseErr := parse(req, declared)
		if parseErr != nil {
			return parseErr
		}

		// Writing into a collection is a write on the collection, and the
		// predicate decides it. An app's own principal reads 'owner' here; a
		// guest acting for anyone else needs a grant.
		guard := a.store.GuardTx(tx)
		if _, err := guard.Authorize(ctx, req.Caller.Credential,
			collectionSubject(info.ID, d.Collection), AccessWrite, "storage.insert"); err != nil {
			return denied(err, false)
		}

		doc := d.Doc
		if len(doc) == 0 {
			doc = json.RawMessage(`{}`)
		}
		ref := d.Ref
		if ref == "" {
			ref = uuid.NewString()
		}
		kind := d.Kind
		if kind == "" {
			kind = d.Collection
		}

		// The invocation's taint, never trusted-by-default. This is the last
		// layer that can get invariant 9 wrong after every other one got it
		// right.
		level := writeTrust(trust.Trusted, req.Trust)

		owner := Owner{Kind: req.Caller.PrincipalKind, ID: req.Caller.PrincipalID}
		var id uuid.UUID
		var created time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO entities (kind, install_id, collection, ref,
			                      owner_kind, owner_id, author_actor, trust, tainted_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING id, created_at`,
			kind, info.ID, d.Collection, ref,
			string(owner.Kind), owner.ID, req.Caller.ActorID, string(level), nullable(req.TaintedBy),
		).Scan(&id, &created); err != nil {
			return fmt.Errorf("insert entity: %w", err)
		}

		if _, docErr := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (id, doc, trust, tainted_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$5)`, table(info.Schema, d.Collection)),
			id, doc, string(level), nullable(req.TaintedBy), created); docErr != nil {
			return fmt.Errorf("insert document: %w", docErr)
		}

		if err := a.emit(ctx, tx, req, info, d.Collection, id, owner, level, "created"); err != nil {
			return err
		}

		body, err := json.Marshal(map[string]any{"id": id, "ref": ref, "created_at": created})
		if err != nil {
			return fmt.Errorf("marshal insert result: %w", err)
		}
		// The RESULT is host-generated, so it is trusted whatever the document
		// is. The document's own trust is on its row and comes back on a read.
		out = wasmhost.Trusted(body)
		return nil
	})
	if txErr != nil {
		return wasmhost.Response{}, txErr
	}
	return out, nil
}

// Get returns one document.
//
// The response's trust is the ROW's, never the request's. "The caller asked for
// trusted data, so return Trusted" reads as reasonable and is a laundering
// machine.
func (a *AppData) Get(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	info, declared, err := resolveInstall(ctx, a.store.Pool(), req.Caller.InstallID)
	if err != nil {
		return wasmhost.Response{}, err
	}
	d, err := parse(req, declared)
	if err != nil {
		return wasmhost.Response{}, err
	}

	var (
		id  uuid.UUID
		row docRow
	)
	switch {
	case d.ID != "":
		if id, err = d.docID(); err != nil {
			return wasmhost.Response{}, err
		}
	case d.Ref != "":
		err = a.store.Pool().QueryRow(ctx,
			"SELECT id FROM entities WHERE install_id = $1 AND collection = $2 AND ref = $3 AND deleted_at IS NULL",
			info.ID, d.Collection, d.Ref).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Same status as "you may not see it": telling them apart is an
			// existence oracle.
			return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusNotFound, "no such document")
		}
		if err != nil {
			return wasmhost.Response{}, fmt.Errorf("resolve ref: %w", err)
		}
	default:
		return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "id or ref is required")
	}

	guard := a.store.Guard()
	if _, authErr := guard.Authorize(ctx, req.Caller.Credential, entitySubject(id), AccessRead, "storage.get"); authErr != nil {
		return wasmhost.Response{}, denied(authErr, true)
	}

	row, err = a.readDoc(ctx, info, d.Collection, id)
	if err != nil {
		return wasmhost.Response{}, err
	}
	body, err := json.Marshal(row)
	if err != nil {
		return wasmhost.Response{}, fmt.Errorf("marshal document: %w", err)
	}
	return wasmhost.Response{Trust: row.Trust, Data: body}, nil
}

func (a *AppData) readDoc(ctx context.Context, info installInfo, collection string, id uuid.UUID) (docRow, error) {
	var (
		row       docRow
		level     string
		taintedBy *string
	)
	err := a.store.Pool().QueryRow(ctx, fmt.Sprintf(`
		SELECT t.id, e.ref, e.kind, t.doc, t.trust, t.tainted_by, t.created_at, t.updated_at
		  FROM %s t
		  JOIN entities e ON e.id = t.id
		 WHERE t.id = $1 AND e.deleted_at IS NULL`, table(info.Schema, collection)), id).
		Scan(&row.ID, &row.Ref, &row.Kind, &row.Doc, &level, &taintedBy, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, wasmhost.Errorf(wasmhost.StatusNotFound, "no such document")
	}
	if err != nil {
		return row, fmt.Errorf("read document: %w", err)
	}
	row.Trust = trust.Level(level)
	if taintedBy != nil {
		row.TaintedBy = *taintedBy
	}
	return row, nil
}

// Update replaces a document's body.
func (a *AppData) Update(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	var out wasmhost.Response
	txErr := a.store.InTx(ctx, func(tx pgx.Tx) error {
		info, declared, resolveErr := resolveInstall(ctx, tx, req.Caller.InstallID)
		if resolveErr != nil {
			return resolveErr
		}
		d, parseErr := parse(req, declared)
		if parseErr != nil {
			return parseErr
		}
		id, idErr := d.docID()
		if idErr != nil {
			return idErr
		}

		guard := a.store.GuardTx(tx)
		if _, err := guard.Authorize(ctx, req.Caller.Credential,
			entitySubject(id), AccessWrite, "storage.update"); err != nil {
			return denied(err, true)
		}

		var existing string
		if err := tx.QueryRow(ctx, "SELECT trust FROM entities WHERE id = $1 AND deleted_at IS NULL", id).
			Scan(&existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return wasmhost.Errorf(wasmhost.StatusNotFound, "no such document")
			}
			return fmt.Errorf("read existing trust: %w", err)
		}
		level := writeTrust(trust.Level(existing), req.Trust)

		doc := d.Doc
		if len(doc) == 0 {
			doc = json.RawMessage(`{}`)
		}

		// updated_at is also maintained by a BEFORE UPDATE trigger in the app's
		// own schema, so setting it here is belt to that brace. It is not
		// redundant reasoning though: the trigger is what makes the column
		// always right, including for a writer that forgets, and this makes the
		// behaviour the same whether or not a given schema has it yet.
		var updated time.Time
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			UPDATE %s SET doc = $2, trust = $3, tainted_by = coalesce($4, tainted_by), updated_at = now()
			 WHERE id = $1
			 RETURNING updated_at`, table(info.Schema, d.Collection)),
			id, doc, string(level), nullable(req.TaintedBy)).Scan(&updated); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return wasmhost.Errorf(wasmhost.StatusNotFound, "no such document")
			}
			return fmt.Errorf("update document: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE entities SET trust = $2, tainted_by = coalesce($3, tainted_by), updated_at = now()
			 WHERE id = $1`, id, string(level), nullable(req.TaintedBy)); err != nil {
			return fmt.Errorf("update entity: %w", err)
		}

		owner := Owner{Kind: req.Caller.PrincipalKind, ID: req.Caller.PrincipalID}
		if err := a.emit(ctx, tx, req, info, d.Collection, id, owner, level, "updated"); err != nil {
			return err
		}

		body, err := json.Marshal(map[string]any{"id": id, "updated_at": updated, "trust": level})
		if err != nil {
			return fmt.Errorf("marshal update result: %w", err)
		}
		out = wasmhost.Trusted(body)
		return nil
	})
	if txErr != nil {
		return wasmhost.Response{}, txErr
	}
	return out, nil
}

// Delete removes a document.
//
// Deleting requires OWNERSHIP, not write access. Sharing is not transfer
// (D13.10): a grantee reads and replies, and editing or deleting someone else's
// memory is not sharing. An override cannot reach this either, because override
// grants are read-only by CHECK.
func (a *AppData) Delete(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	var out wasmhost.Response
	txErr := a.store.InTx(ctx, func(tx pgx.Tx) error {
		info, declared, resolveErr := resolveInstall(ctx, tx, req.Caller.InstallID)
		if resolveErr != nil {
			return resolveErr
		}
		d, parseErr := parse(req, declared)
		if parseErr != nil {
			return parseErr
		}
		id, idErr := d.docID()
		if idErr != nil {
			return idErr
		}

		guard := a.store.GuardTx(tx)
		reason, authErr := guard.Authorize(ctx, req.Caller.Credential, entitySubject(id), AccessWrite, "storage.delete")
		if authErr != nil {
			return denied(authErr, true)
		}
		if reason != ReasonOwner {
			return wasmhost.Errorf(wasmhost.StatusDenied,
				"deleting is the owner's act; %s is not ownership (D13.10)", reason)
		}

		if _, docErr := tx.Exec(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE id = $1", table(info.Schema, d.Collection)), id); docErr != nil {
			return fmt.Errorf("delete document: %w", docErr)
		}
		tag, entErr := tx.Exec(ctx, "DELETE FROM entities WHERE id = $1", id)
		if entErr != nil {
			return fmt.Errorf("delete entity: %w", entErr)
		}
		if tag.RowsAffected() == 0 {
			return wasmhost.Errorf(wasmhost.StatusNotFound, "no such document")
		}

		owner := Owner{Kind: req.Caller.PrincipalKind, ID: req.Caller.PrincipalID}
		if err := a.emit(ctx, tx, req, info, d.Collection, id, owner, trust.Trusted, "deleted"); err != nil {
			return err
		}

		body, err := json.Marshal(map[string]any{"id": id, "deleted": true})
		if err != nil {
			return fmt.Errorf("marshal delete result: %w", err)
		}
		out = wasmhost.Trusted(body)
		return nil
	})
	if txErr != nil {
		return wasmhost.Response{}, txErr
	}
	return out, nil
}

// Query lists the documents in a collection this caller may see.
//
// The filter is access_decision() and nothing else. The document table carries
// no owner columns to be tempted by ... ownership lives on the entities row
// this joins to, so there is no cheaper copy for a later query to filter on by
// mistake.
func (a *AppData) Query(ctx context.Context, req wasmhost.Request) (wasmhost.Response, error) {
	info, declared, err := resolveInstall(ctx, a.store.Pool(), req.Caller.InstallID)
	if err != nil {
		return wasmhost.Response{}, err
	}
	d, err := parse(req, declared)
	if err != nil {
		return wasmhost.Response{}, err
	}

	limit := d.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	match := d.Match
	if len(match) == 0 {
		match = json.RawMessage(`{}`)
	}
	var after any
	if d.After != "" {
		id, parseErr := uuid.Parse(d.After)
		if parseErr != nil {
			return wasmhost.Response{}, wasmhost.Errorf(wasmhost.StatusInvalid, "after %q is not a uuid", d.After)
		}
		after = id
	}

	rows, err := a.store.Pool().Query(ctx, fmt.Sprintf(`
		SELECT t.id, e.ref, e.kind, t.doc, t.trust, t.tainted_by, t.created_at, t.updated_at
		  FROM %s t
		  JOIN entities e ON e.id = t.id
		 WHERE e.deleted_at IS NULL
		   AND ($1 = '' OR e.kind = $1)
		   AND t.doc @> $2::jsonb
		   AND ($3::uuid IS NULL OR t.created_at < (SELECT created_at FROM entities WHERE id = $3))
		   AND access_reason('entity', e.id, NULL, $4, $5, $6, 'read', now()) IS NOT NULL
		 ORDER BY t.created_at DESC, t.id DESC
		 LIMIT $7`, table(info.Schema, d.Collection)),
		d.Kind, match, after,
		string(req.Caller.PrincipalKind), req.Caller.PrincipalID, req.Caller.ActorID, limit)
	if err != nil {
		return wasmhost.Response{}, fmt.Errorf("query documents: %w", err)
	}
	defer rows.Close()

	out := make([]docRow, 0, limit)
	// A batch containing untrusted content taints the invocation that read it.
	// Anything weaker would let a guest launder by reading in bulk.
	level := trust.Trusted
	for rows.Next() {
		var (
			row       docRow
			rowTrust  string
			taintedBy *string
		)
		if scanErr := rows.Scan(&row.ID, &row.Ref, &row.Kind, &row.Doc, &rowTrust,
			&taintedBy, &row.CreatedAt, &row.UpdatedAt); scanErr != nil {
			return wasmhost.Response{}, fmt.Errorf("scan document: %w", scanErr)
		}
		row.Trust = trust.Level(rowTrust)
		if taintedBy != nil {
			row.TaintedBy = *taintedBy
		}
		level = writeTrust(level, row.Trust)
		out = append(out, row)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return wasmhost.Response{}, fmt.Errorf("query documents: %w", rowsErr)
	}

	var next any
	if len(out) == limit {
		next = out[len(out)-1].ID
	}
	body, err := json.Marshal(map[string]any{"rows": out, "next": next})
	if err != nil {
		return wasmhost.Response{}, fmt.Errorf("marshal query result: %w", err)
	}
	return wasmhost.Response{Trust: level, Data: body}, nil
}

// emit writes the event announcing a write, in the same transaction as the
// write itself (D14.2: with no second writer, every write produces an event, so
// mentions fire by construction).
func (a *AppData) emit(ctx context.Context, tx pgx.Tx, req wasmhost.Request, info installInfo,
	collection string, id uuid.UUID, owner Owner, level trust.Level, verb string,
) error {
	ev := &Event{
		Kind:          fmt.Sprintf("%s.%s.%s", info.Slug, collection, verb),
		Subject:       entitySubject(id),
		Owner:         owner,
		AuthorActor:   req.Caller.ActorID,
		PrincipalKind: req.Caller.PrincipalKind,
		PrincipalID:   req.Caller.PrincipalID,
		Body:          json.RawMessage(fmt.Sprintf(`{"id":%q,"collection":%q}`, id, collection)),
		Trust:         string(level),
	}
	if err := AppendEvents(ctx, tx, ev); err != nil {
		return fmt.Errorf("emit %s: %w", ev.Kind, err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
