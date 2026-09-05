# Blob layer

The one content-addressed store for every byte in the platform (D11): uploads,
screenshots, compiled guest modules, guest source, harness transcripts, stream
spools, oversized workflow step outputs. One store with classes, not a module
store beside a blob store.

Two halves. The **driver** stores bytes at a content address and knows nothing
else. The **catalog** owns the `blobs` and `blob_refs` rows, and is where
ownership, permission and trust live. The last section says what is still
missing.

## The address has no owner in it

A blob is addressed by `<hh>/<sha256>`. Two tenants uploading identical bytes
get one object.

**Ownership, permission and trust are properties of a reference, not of bytes**
(invariant 3, D17.1). The merged schema says the same thing structurally:
`blobs` is keyed by `sha256` alone; `blob_refs` carries `owner_kind`,
`owner_id`, `author_actor` and `trust`.

This is worth stating loudly because the other design is the natural one, and
the arguments for it are good. Putting the owner in the key appears to buy:

- **Safe deletion**, since "does anything still reference these bytes" becomes
  answerable per tenant. Not needed: the refcount is live rows in `blob_refs`
  for that hash across **all** owners, which is what `blob_refs_hash_idx`
  exists for. Scoping that query to one tenant is the thing that would make it
  wrong.
- **A guest that learns another tenant's hash addressing nothing** rather than
  something forbidden — absence beats denial, no oracle, no timing signal.
  That property is real and worth keeping, and it comes from `host.blob.read`
  resolving through the **caller's refs** rather than the global hash space. It
  does not come from the physical key.

The cost of the owner-in-key design is that dedup becomes per tenant. For a
household re-importing a photo library that is the entire transfer, twice.

So: **global bytes, per-reference everything else.**

## The split that makes crashes recoverable

**Postgres is the authority on what exists. The driver is the authority on what
the bytes are.** Neither is asked the other's question.

A driver never consults a database and never decides who may read anything. It
stores, returns and deletes bytes at a content address. That is what lets the
crash window between "bytes written" and "row live" fail toward reclaimable
litter instead of a live row pointing at nothing.

## Verification happens once

`Upload.Seal` hashes every byte and refuses to publish anything that does not
match a declared hash. After it succeeds, the digest is a fact about the object.

**A ranged read is not hash-verified and cannot be.** A range is a slice and the
digest is over the whole object. Verification is an ingest-completion property,
never re-established per read. Code that "verifies" a partial read is either
hashing the wrong thing or reading the whole object to check a slice of it.

The declared hash is a hint, never trusted. Every client keeps a
content-addressed cache (D6.2), so it has already hashed the file before
deciding to upload; handing that over first makes a dedup hit cost zero bytes
and sends the bytes straight to their final address. The driver still hashes
everything and `Seal` returns `*DigestMismatch` on disagreement.

## Delivery: why scriptable types can never be redirected

`PlanDelivery` returns proxy or redirect, and it is the only place that decides.

Serving user-supplied HTML safely needs `X-Content-Type-Options: nosniff`.
**S3 has no response-header override for nosniff.** It can override
content-type and content-disposition; it cannot add that one. So a signed URL
*structurally cannot* carry the header that stops a browser sniffing bytes into
script.

Therefore anything a browser can execute is proxied by the host, which sets its
own headers. The list in `ScriptableMIME` is deliberately generous and includes
`image/svg+xml`, `application/pdf` and everything ending in `+xml`. An
unparseable type counts as scriptable: absence of information is not permission.

The disk driver cannot presign at all, so for as long as it was the only driver,
everything was proxied and this rule was a claim nobody had tested. The S3
driver is where it does work, and it enforces the rule twice: `Deliver` consults
`PlanDelivery`, and `PresignGet` refuses a scriptable type independently. A
caller that skips `PlanDelivery`, or a `PlanDelivery` that grows a bug, still
cannot get scriptable bytes signed.

## Ranges

`Range.Clamp` resolves a request against a known size:

- A zero range is the whole object.
- A zero length means "to the end", like a `Range` header with no end position.
- A window past the end is truncated, not refused.
- An offset **at or past** the end is `ErrRangeNotSatisfiable` — a 416, not an
  empty 206.

**Never emit a multi-range request.** A store answering one returns 200 with the
entire body, not 206 with parts, so a downloader that asks for several ranges at
once and expects partial content gets the whole object and no error to notice it
by. Ask for one range at a time.

Two more downloader rules that belong with it, for whoever writes that half:
**do not key stale-URL retry on 403** (an expired signed URL is not reliably a
403, and a real permission failure is not reliably retryable), and **do not
expect the store to verify partials**, per the section above.

## Durability classes

Captured at ingest and recorded on the blobs row. **Evict and delete are
different operations**: evicting drops bytes the host can rebuild, deleting
drops bytes that are gone.

| Class | Meaning | Evictable |
| --- | --- | --- |
| `derived` | regenerable from another blob plus a recipe: a thumbnail, a transcode | yes |
| `build` | a compiled artifact, regenerable from pinned source | yes |
| `capture` | a screenshot of a page that has changed, a transcript, a fetched document | **no** |
| `original` | the only copy of something a person gave us | **no** |

A class alone is not enough to evict: the row must also carry a source hash and
a recipe, which the schema enforces with a CHECK constraint. Otherwise the host
drops bytes believing it can get them back and then cannot say from what.

## The disk driver

`<root>/<hh>/<sha256>`, with in-progress uploads under `<root>/tmp` so publish
is a rename within one filesystem and therefore atomic. `fsync` before the
rename, because otherwise a crash can leave a correctly named file whose
contents were never flushed — and content addressing makes that look valid
forever.

`SweepExpiredUploads` reclaims abandoned temp files and **returns errors rather
than swallowing them**. A sweeper that reclaims nothing while reporting success
is how a disk fills up quietly. Its cutoff must be older than the longest
legitimate idle period: a guest append can span workflow steps.

Published objects are never swept by the driver. Whether those may go is a
question about refs.

## The reference layer

`Catalog` is the half that knows about owners. It writes the `blobs` and
`blob_refs` rows and ties them to bytes.

**`Publish` takes a `pgx.Tx`, not a `DB`.** "No blob exists without a ref" is
only true if the row and the reference cannot be written separately, and a
signature accepting a pool would let a caller separate them by accident. The
type is the enforcement, and `no_live_blob_without_a_ref` proves the rollback.

**`Resolve` looks through the caller's own references and never at the global
hash space.** A caller holding a hash it has no reference to gets `NotFound`
— the same error as a hash that was never stored. That is what makes absence
beat denial: no oracle, no timing difference, no policy to get wrong. It is also
the property that lets the physical key stay owner-free.

**Trust rides the reference** (`crates/hive-trust`), not the bytes. An upload and a
fetched page with identical bytes are one `blobs` row and two references that
honestly disagree, and re-referencing can only move trust downward — otherwise
global dedup would launder web content into trusted.

**Whatever produced a blob writes its ref, host-internal producers included.**
`SourceKind` lists every one: modules, guest source, transcripts, spools,
screenshots, step outputs, harness diffs. A sweeper that does not know about a
producer deletes that producer's output, so the schema CHECK and the Rust enum
are two halves of one rule.

### Collection

**`blob` is a reserved key in every document body. If you are writing an app,
this is the one rule here you need before you hit it.**

A document may name blobs, and the host maintains the references for you: it
walks each document handed to `host.storage.insert`, `update` and `delete`, and
writes, moves or releases `blob_refs` rows on the same transaction as the
document. That is a requirement rather than a convenience — without it either
blobs leak forever or a delete collects bytes out from under a live document.

The host recognises a descriptor by the key alone:

```json
{
  "title": "a day out",
  "cover": { "blob": "<64 hex>", "size": 20481, "mime": "image/jpeg" },
  "attachments": [ { "note": "receipt", "file": { "blob": "<64 hex>", "size": 812 } } ]
}
```

Nesting and arrays are walked, so a descriptor anywhere in the document counts.

**A `blob` key whose value is not a 64-character hex digest fails the write**,
with a message naming the JSON path. It is not ignored, and that asymmetry is
deliberate: ignoring it would mean a single mistyped character in a digest
silently stops being a descriptor and becomes an ordinary string field, so the
bytes it names lose their reference and are collected later, with nothing
connecting the corruption to the write that caused it. Failing loudly on the
call that caused it is the cheaper mistake by a wide margin.

So: **use any other key for a checksum, a content address you are merely
recording, or anything else that happens to be a hex string.** `sha256`,
`digest`, `checksum` are all ordinary fields. Only `blob` is claimed.

Two more rules that follow from ownership living on the reference (see above):

- A document can only name blobs **its own principal already holds a live
  reference to**. Naming somebody else's returns the same "not found" as naming
  a digest that was never stored — the two are indistinguishable on purpose,
  because telling them apart would let a caller probe the hash space.
- A reference the host writes for you can never be **more** trusted than the
  write that created it or the reference you already held. A document written
  during an untrusted invocation holds its blobs untrusted.

`Unreferenced` lists live blobs with no live reference **across every owner**.
Scoping that count per tenant is precisely what would let one owner's last
release unlink bytes another still holds — the failure the owner-in-the-key
design was invented to prevent, and which it would have had to introduce first.

Being a candidate is not permission to delete. `Trash` re-checks under the row
lock and returns false if a reference appeared in between. The row flips to
`trashed` inside the transaction and the bytes go after it commits: deleting
bytes first would leave a live row pointing at nothing, which reads as
corruption, while this order leaves at worst a trashed row whose bytes the next
sweep clears. `DeleteTrashedBytes` refuses any other state, which stops a caller
reaching past the reference check by calling the driver directly.

## The S3 driver

`S3Driver` targets any S3-compatible store; Garage is the one it is developed
and tested against. Path-style addressing (Garage serves one endpoint for every
bucket), a fixed region that must match `s3_region` in the server config because
SigV4 puts it in the credential scope, and an optional key prefix so one bucket
can hold more than this platform.

**The prefix is not a tenant boundary.** Two principals who upload the same
bytes still land on the same key; who may read them is a `blob_refs` row. A
per-owner prefix would be invariant 3 for the sixth time.

Uploads **buffer to a local temp file** rather than streaming through. The
address is the digest and the digest is not known until the last byte, so
something has to hold the bytes. The alternative — stream to a temp key, then
copy server-side on seal — costs an O(size) copy on every upload; buffering
costs local disk, once.

### What running it against a real server settled

These were assumptions until Garage answered them, and three of them were wrong
in a way that would have shipped:

| Claim | What a live Garage does |
| --- | --- |
| An expired signature returns 403 | **400.** `MaybeExpired` covers 400, 401 and 403 for this reason; a client that retried only on 403 would surface an expiry as a hard failure |
| A `Range` against a presigned URL is refused | **206, byte-exact.** SigV4 query presigning signs `host` and the query; `Range` is an unsigned request header, so one signed URL can be fetched whole or in pieces |
| A ranged GET carries a checksum to verify against | It does not. No `x-amz-checksum-*` on a ranged GET, which is why partial reads are documented as unverifiable rather than verified loosely |
| `garage layout assign --single-node` sets up a dev cluster | Not in v2.3.0. The layout is assigned to an explicit node id and applied at an explicit version, which is what `scripts/garage-up` does |

Garage itself sits at about 3.2 MB RSS at rest and does not buffer objects, so
it is cheap enough to leave running on a development machine.

### Running it

```powershell
.\scripts\garage-up.ps1     # prints the four env vars the tests read
.\scripts\garage-down.ps1   # -Purge also deletes the volumes
```

```bash
./scripts/garage-up.sh
./scripts/garage-down.sh
```

The script is idempotent and does four things the container alone does not: it
generates an RPC secret (gitignored, never committed), waits for the node to
answer, applies a layout, and mints a key. **A Garage with no layout accepts
connections and then refuses every write**, which looks exactly like a broken
driver, so the script is the supported way in.

The S3 tests skip when those variables are unset, and the `blobstore` CI job
sets `HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS=1` so a skip there is a failure. That
job runs `garage-up.sh` rather than a service container, so the script
developers depend on is itself under test.

## What this does not include yet

Multipart upload (every object is a single PUT today, so a very large blob holds
a very large spool file), eviction — `StateEvicted` exists and the class rules
are enforced, but nothing evicts yet — and any sweep of abandoned objects on the
S3 side, which the disk driver has and this does not.
