package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/bees-roadhouse/hive-sandbox/internal/blob"
	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// Bounds on the walk. A document arrives from a guest, so both of these are
// limits on what a guest can make the host do rather than tuning.
//
// Neither truncates. Past a bound the write is refused, because a document that
// silently kept only the first 256 of its descriptors would have references for
// some of its blobs and not others, and the ones without would be collected out
// from under a live document ... which is the exact failure this whole file
// exists to prevent.
const (
	maxDescriptorDepth   = 64
	maxDescriptorsPerDoc = 256
)

// descriptorKey is the field a blob descriptor announces itself with, and it is
// RESERVED in a document body.
//
// It is the wire name Descriptor.MarshalJSON emits, so an app that stores a
// descriptor stores this key whether or not it meant to. An app that uses it for
// anything else gets its write refused, naming the path ... see the reserved-key
// rule in docs/blob.md, which is where an app author reads it.
const descriptorKey = "blob"

// descriptorsIn returns every blob a document names, sorted and deduplicated.
//
// # Why the match is loose
//
// A descriptor on the wire is {"blob": "<64 hex>", "size": N, "mime": "..."},
// and the obvious tighter rule is to require all three. That rule was written
// and rejected, because the two ways to be wrong here are not symmetric:
//
//   - Matching too little is silent and permanent. A blob a live document names
//     gets no reference, so it is unreferenced, so it is collected ... and the
//     document is corrupt at some later date with nothing connecting the two
//     events. A guest that hand-wrote {"blob": "<hash>"} rather than going
//     through the SDK would land exactly here.
//   - Matching too much is loud and immediate. The write fails with a status
//     the caller sees, on the call that caused it.
//
// So this matches on the presence of "blob" alone, and a value under it that is
// not a 64-character hex digest is REFUSED rather than ignored. Ignoring it
// would put the false-negative back: a typo in a digest would stop being a
// descriptor and start being an ordinary field, silently, which is the exact
// direction that loses bytes. Refusing keeps the loud-and-immediate property
// and turns the reserved word into something an app author learns the first
// time rather than the hard way.
//
// The refusal names the JSON path, because "blob is reserved" is not actionable
// without knowing which one.
//
// Sorted because map iteration is not ordered, and the order decides which of
// several failures a caller is told about first. A caller retrying a failed
// write should get the same answer twice.
func descriptorsIn(doc json.RawMessage) ([]blob.Hash, error) {
	if len(doc) == 0 {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader(doc))
	// UseNumber so a large size never round-trips through float64. Nothing here
	// reads size today; the decoder setting is what stops that from becoming a
	// silent precision bug when something does.
	dec.UseNumber()

	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, wasmhost.Errorf(wasmhost.StatusInvalid, "document is not json: %v", err)
	}

	var (
		out  []blob.Hash
		seen = make(map[blob.Hash]bool)
	)

	var walk func(node any, path string, depth int) error
	walk = func(node any, path string, depth int) error {
		if depth > maxDescriptorDepth {
			return wasmhost.Errorf(wasmhost.StatusInvalid,
				"%s: document nests deeper than %d levels", path, maxDescriptorDepth)
		}
		switch v := node.(type) {
		case map[string]any:
			if raw, present := v[descriptorKey]; present {
				h, err := descriptorHash(raw)
				if err != nil {
					return wasmhost.Errorf(wasmhost.StatusInvalid,
						"%s.%s: %q is reserved for blob descriptors and must be a "+
							"64-character hex digest (%v)",
						path, descriptorKey, descriptorKey, err)
				}
				if !seen[h] {
					if len(out) >= maxDescriptorsPerDoc {
						return wasmhost.Errorf(wasmhost.StatusInvalid,
							"document names more than %d blobs", maxDescriptorsPerDoc)
					}
					seen[h] = true
					out = append(out, h)
				}
			}
			// Keep descending even through an object that already matched. A
			// well-formed descriptor holds only scalars so this costs nothing,
			// and stopping would let a nested descriptor hide inside a
			// malformed one.
			// Sorted, for the same reason the result is sorted: map iteration
			// is not ordered, so a document with two bad paths would name a
			// different one each time and a caller retrying a failed write
			// would get a different answer twice.
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := walk(v[key], path+"."+key, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for i, child := range v {
				if err := walk(child, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(root, "doc", 0); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// descriptorHash reads the hash out of the value under the reserved key.
//
// Every failure is an error rather than a "not a descriptor", including a value
// of the wrong JSON type. A number or an object under this key is not an
// innocent field the walk should skip ... it is the reserved word being used for
// something else, which the author needs telling about.
func descriptorHash(raw any) (blob.Hash, error) {
	s, ok := raw.(string)
	if !ok {
		return blob.Hash{}, fmt.Errorf("value is %T, not a string", raw)
	}
	return blob.ParseHash(s)
}
