package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The schema plan: everything about an app's storage that can be decided from
// its manifest, in a form that has no free text left in it.
//
// That last part is the whole point. An index declaration ends up inside a
// CREATE INDEX statement, and a manifest is a file an AI writes. Handing a
// string straight from a manifest to the thing that builds DDL is injection
// with extra steps, and no amount of care at the DDL end fixes it, because by
// then the dangerous value looks exactly like a legitimate one.
//
// So declarations are parsed into a closed set of methods and validated path
// segments here, once, and whatever executes the DDL receives structure rather
// than syntax. It cannot interpolate what it was never given.

// IndexMethod is a Postgres index type an app may ask for. Closed set: an app
// naming a method the host does not know is refused rather than passed through.
type IndexMethod string

const (
	// IndexBTree orders and ranges. The default for timestamps and ids.
	IndexBTree IndexMethod = "btree"
	// IndexGIN indexes containment, which is how tag arrays get searched.
	IndexGIN IndexMethod = "gin"
	// IndexFTS is full text over a document path.
	IndexFTS IndexMethod = "fts"
	// IndexVector is semantic recall. It carries a dimension.
	IndexVector IndexMethod = "vector"
)

func (m IndexMethod) known() bool {
	switch m {
	case IndexBTree, IndexGIN, IndexFTS, IndexVector:
		return true
	default:
		return false
	}
}

// Index is one parsed declaration.
type Index struct {
	Method IndexMethod
	// Path is the document path, already split and validated segment by
	// segment. A slice rather than a dotted string, so a consumer builds the
	// JSON accessor itself and never parses this again.
	Path []string
	// Dim is the vector dimension. Zero for every other method.
	Dim int
}

// String renders the canonical form, which is also the form a manifest writes.
func (i Index) String() string {
	p := strings.Join(i.Path, ".")
	if i.Method == IndexVector {
		return fmt.Sprintf("vector(%s, %d)", p, i.Dim)
	}
	return fmt.Sprintf("%s(%s)", i.Method, p)
}

// segmentRE bounds a single document path segment. Same reasoning as nameRE:
// these become JSON keys and appear inside generated SQL, and the set that is
// safe in both is small.
var segmentRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// indexRE splits `method(args)` without interpreting args.
var indexRE = regexp.MustCompile(`^([a-z]+)\(([^()]*)\)$`)

// maxVectorDim is above every embedding model worth naming and below anything
// pgvector will accept for an indexable column. A dimension is a number in
// generated DDL, so it is bounded rather than trusted.
const maxVectorDim = 4096

// ParseIndex turns one manifest declaration into structure, or refuses.
func ParseIndex(decl string) (Index, error) {
	m := indexRE.FindStringSubmatch(strings.TrimSpace(decl))
	if m == nil {
		return Index{}, fmt.Errorf("%w: %q is not method(path)", ErrIndex, decl)
	}

	method := IndexMethod(m[1])
	if !method.known() {
		return Index{}, fmt.Errorf("%w: unknown method %q in %q", ErrIndex, method, decl)
	}

	args := strings.Split(m[2], ",")
	pathArg := strings.TrimSpace(args[0])

	idx := Index{Method: method}
	switch method {
	case IndexVector:
		if len(args) != 2 {
			return Index{}, fmt.Errorf(
				"%w: %q needs a dimension, as vector(path, 1536)", ErrIndex, decl)
		}
		dim, err := strconv.Atoi(strings.TrimSpace(args[1]))
		if err != nil || dim < 1 || dim > maxVectorDim {
			return Index{}, fmt.Errorf("%w: %q has a dimension outside 1..%d",
				ErrIndex, decl, maxVectorDim)
		}
		idx.Dim = dim
	default:
		if len(args) != 1 {
			return Index{}, fmt.Errorf("%w: %q takes one path", ErrIndex, decl)
		}
	}

	if pathArg == "" {
		return Index{}, fmt.Errorf("%w: %q has an empty path", ErrIndex, decl)
	}
	for _, seg := range strings.Split(pathArg, ".") {
		seg = strings.TrimSpace(seg)
		if !segmentRE.MatchString(seg) {
			return Index{}, fmt.Errorf("%w: %q has an unusable path segment %q",
				ErrIndex, decl, seg)
		}
		idx.Path = append(idx.Path, seg)
	}
	return idx, nil
}

// SchemaPlan is what the host provisions for one install.
type SchemaPlan struct {
	// Schema is the Postgres schema name. The host derives it; an app never
	// names it, and uninstall is DROP SCHEMA on exactly this (D3.2).
	Schema      string
	Collections []CollectionPlan
}

// CollectionPlan is one collection and its parsed indexes.
type CollectionPlan struct {
	Name    string
	CRUD    bool
	Indexes []Index
}

// maxIdentifier is Postgres's limit. An identifier longer than this is silently
// TRUNCATED rather than rejected, which is the dangerous half: two apps whose
// names differ only past the limit would quietly share a schema.
const maxIdentifier = 63

// schemaPrefix is the namespace every app schema lives under, so a schema the
// host provisioned is distinguishable from one somebody made by hand.
const schemaPrefix = "app_"

// SchemaName is where ONE INSTALL's collections live.
//
// Scoped to the owner, not just the app, and that was a real bug rather than a
// refinement: an earlier version returned `app_<slug>`, which is per-APP. But
// `installs.schema_name` is UNIQUE and the data layer reads the schema off the
// install row, so two owners installing the same app either collided on that
// constraint or ... had the constraint not existed ... would have shared one
// schema and therefore each other's documents.
//
// It failed closed, which is the only reason this is a story about a unique
// index rather than about a cross-principal data leak.
//
// Derived from (slug, owner) rather than from the install id, because the
// schema has to be provisionable BEFORE the install row exists, and because
// re-installing the same app for the same owner must land on the same schema
// for a manifest diff to be a migration rather than a second copy.
func SchemaName(app string, ownerKind string, ownerID string) string {
	sum := sha256.Sum256([]byte(ownerKind + ":" + ownerID))
	return schemaPrefix + app + "_" + hex.EncodeToString(sum[:])[:ownerSuffixLen]
}

// ownerSuffixLen is how much of the owner digest lands in the schema name. Eight
// hex characters is 32 bits: at family scale a collision is not a rounding
// error, it is not going to happen, and `schema_name UNIQUE` catches it loudly
// if it ever does.
const ownerSuffixLen = 8

// maxAppName is what fits once the prefix is applied. Enforced in Validate so
// the failure lands on the manifest rather than on the CREATE SCHEMA, which is
// the exact "legal in a manifest, illegal in Postgres" gap that validating
// names narrowly was supposed to close in the first place.
const maxAppName = maxIdentifier - len(schemaPrefix) - 1 - ownerSuffixLen

// derivedSuffixes are every suffix the platform appends to a COLLECTION name
// when it derives an identifier. The list is here rather than in the package
// that builds them, because the name has to be bounded where it is validated
// and a bound is only correct if it knows every suffix.
//
// This is the sibling of the maxAppName bug and it is worse, because it fails
// silently: bounding the inputs at 63 and forgetting the derived names meant
// `<collection>_owner_idx` truncated back onto the collection name, collided
// with the table in pg_class, and IF NOT EXISTS reported that collision as
// success. The install said yes and no index existed ... including the owner
// index every grant-filtered read depends on.
//
// The ordinal is padded to four digits' worth of room, so an app with a
// thousand indexes on one collection is bounded by the same arithmetic rather
// than by luck.
var derivedSuffixes = []string{
	"_owner_idx",       // applyCollection
	"_touch",           // the updated_at trigger
	"_vector_9999_idx", // applyIndex, longest method and ordinal
}

// longestDerivedSuffix is the reservation every collection name pays.
func longestDerivedSuffix() int {
	longest := 0
	for _, s := range derivedSuffixes {
		if len(s) > longest {
			longest = len(s)
		}
	}
	return longest
}

// MaxCollectionName is the longest collection name whose derived identifiers
// all still fit. Exported because the tests that keep this honest live in two
// packages, and a duplicated constant would drift.
func MaxCollectionName() int { return maxIdentifier - longestDerivedSuffix() }

// MaxIdentifier is Postgres's identifier limit, exported for the same reason.
func MaxIdentifier() int { return maxIdentifier }

// MaxAppName is the longest app name whose derived schema name still fits, and
// therefore still keeps its owner digest.
//
// Exported because Validate is not the only place that has to know it.
// SchemaName appends the owner digest LAST, so a slug over this bound pushes
// that digest off the end of a Postgres identifier: two owners then derive
// names distinct in Go and identical in Postgres, and `schema_name UNIQUE`
// never sees it, because that column is text and stores the untruncated string
// happily. Anything deriving a schema name from a slug it did not get from
// Validate has to check this itself.
func MaxAppName() int { return maxAppName }

// DerivedSuffixes is every suffix the platform appends to a collection name.
// Exported so a test can assert the budget covers all of them, which is what
// makes adding a longer one fail here rather than in Postgres.
func DerivedSuffixes() []string {
	out := make([]string, len(derivedSuffixes))
	copy(out, derivedSuffixes)
	return out
}

// SchemaPlan derives storage provisioning from a validated manifest.
//
// A tool has no storage by construction (D10.3), so this returns a plan with no
// collections for one, and the registry skips provisioning entirely.
func (m *Manifest) SchemaPlan(ownerKind, ownerID string) (SchemaPlan, error) {
	plan := SchemaPlan{Schema: SchemaName(m.Name, ownerKind, ownerID)}
	if m.Kind != KindApp {
		return plan, nil
	}
	for _, c := range m.Storage.Collections {
		cp := CollectionPlan{Name: c.Name, CRUD: c.CRUD}
		for _, decl := range c.Indexes {
			idx, err := ParseIndex(decl)
			if err != nil {
				return SchemaPlan{}, fmt.Errorf("collection %q: %w", c.Name, err)
			}
			cp.Indexes = append(cp.Indexes, idx)
		}
		plan.Collections = append(plan.Collections, cp)
	}
	return plan, nil
}
