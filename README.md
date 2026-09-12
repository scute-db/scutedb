# ScuteDB

A database engine written from scratch in Go, one layer at a time.

A *scute* is one of the bony plates that make up a turtle's shell. A shell is
made of plates; a database file is made of pages. 


**Status:** Phase 0 complete more to go a lot of learning and doing.

---

## Quick start

```
go test ./...              # everything
make demo                  # list the runnable experiments
make demo-scan             # watch a file-based database degrade
make hexdump               # write real pages and look at the bytes
```

---

## Roadmap I am trying to follow plan first execute second

| Phase | What | Status |
|-------|------|--------|
| **0** | Foundations — interfaces, the naive database, pages | **done** |
| A | Bytes & the B+Tree | **in progress** |
| B | Persistence — storage manager, buffer pool, locking | |
| C | A real data store — schema, rows, indexes | |
| D | Transactions — WAL, recovery, 2PL, MVCC | |
| E | Beyond — LSM engine, Raft, server, query planner | |

---

## Phase 0 — Foundations

### `0x00` Project setup and the three interfaces

Three interfaces were defined before any implementation, so that later work is
addition rather than rewrite.

| Interface | Package | Implemented later by |
|---|---|---|
| `File` | `internal/fileio` | `OSFile` now; mmap / `O_DIRECT` / `io_uring` later |
| `Index` | `internal/index` | B+Tree (`0x06`), bitmap (`0x11`), HNSW (`0x22`) |
| `Engine` | `internal/storage` | heap file (`0x0F`), LSM-tree (`0x1B`) |

Two decisions worth recording:

- **`File` has no `Seek`.** All I/O is positional (`ReadAt` / `WriteAt`). A
  shared file with a cursor cannot be used safely from multiple goroutines,
  and step `0x0C` depends on this.
- **`Engine.Update` returns a new `RowID`.** A record that grows may not fit
  where it was and can be forced to move. Row IDs are not stable, and the
  signature says so rather than leaving it to be discovered.

### `0x01` The naive database, and four ways it fails

`internal/naive` is a database whose entire format is `key\tvalue\n`, appended.
It exists to be measured and thrown away. Four experiments, all reproducible:

**1. Lookup is O(n)** — `make demo-scan`

```
     records     file size     lookup time      per record
      10,000      359.2 KB           721µs            72ns
     100,000        3.7 MB         4.191ms            41ns
     500,000       19.3 MB        13.844ms            27ns
```

Per-record cost flattens, so total time is N × a constant. At 500k records one
lookup costs ~14ms.

**2. Write amplification** — `make demo-update`

Changing one 29-byte record in a 100,000-record database, then reclaiming the
dead space:

```
bytes that changed      29 B
compaction wrote        3.2 MB
write amplification     116,475x
```

**3. Concurrent writers destroy data** — `make demo-race`

Two goroutines sharing a write offset with no lock. Expected 1,000 records:

```
intact records          503
records LOST            497
```

Silent. No error returned. `go test -race` names the exact line.

**4. Buffered data does not survive a crash** — `make demo-crash`

A child process writes records and is `SIGKILL`ed:

```
child reported writing  328,456 records
actually on disk        326,017 records
VANISHED                  2,439 records
```

`Put()` returned `nil` for every one of those. They were in a userspace
`bufio.Writer` and never reached the operating system.

Note the limit of this experiment: data that *did* reach the OS survives
`kill -9` fine, because the kernel still holds it. Losing that requires a power
cut, and defending against it is `fsync`'s job — covered properly in `0x13`.

### `0x02` Pages

Every failure above has the same root cause: no fixed unit. Records of
unpredictable length at unpredictable offsets cannot be found, updated, or
handed to a writer safely.

`internal/page` fixes the unit at **4096 bytes** — matching the APFS/ext4
filesystem block size, and a whole number of 512-byte disk sectors. (It does
*not* match the virtual-memory page size everywhere: x86-64 uses 4 KB but Apple
Silicon uses 16 KB. The filesystem block is the alignment that matters for
I/O.) Postgres uses 8 KB,
SQLite defaults to 4 KB, InnoDB uses 16 KB.

**Page header — 16 bytes, big-endian:**

| offset | size | field | notes |
|---|---|---|---|
| 0 | 4 | page id | caps the database at 2³² pages × 4 KB = 16 TB |
| 4 | 1 | kind | free / meta / heap / btree-leaf / btree-internal |
| 5 | 1 | flags | 8 unused bits |
| 6 | 2 | item count | |
| 8 | 2 | free start | moves as the page fills |
| 10 | 2 | free end | moves down once slots exist (`0x0F`) |
| 12 | 4 | reserved | checksum lands here in `0x13` |

Big-endian is deliberate: it reads correctly in a hexdump, and big-endian
integers sort correctly when compared as raw bytes, which is what makes them
usable as B+Tree keys (`0x03`).

**The payoff** is one line:

```go
func Offset(id core.PageID) int64 { return int64(id) * Size }
```

Reading page 900,000 costs exactly what reading page 0 costs.

**Verify it yourself** — `make hexdump`:

```
00000000  00 00 00 00 01 00 00 01  00 24 10 00 00 00 00 00  |.........$......|
00000010  68 65 6c 6c 6f 20 66 72  6f 6d 20 70 61 67 65 20  |hello from page |
```

`00 24` is 36 — and 16 (header) + 20 (`"hello from page zero"`) = 36. The page
describes itself truthfully. `make demo-header` decodes every field with the
arithmetic shown.

---

## Phase A — Bytes & the B+Tree

### `0x03` Binary serialization

`internal/page` could store bytes but not tell two items apart: appending
`"scute-db"` then `"hello"` produced `"scute-dbhello"` with no recoverable
boundary. `internal/codec` gives bytes structure.

The package is deliberately split in two, because **keys and values have
different jobs**:

| | goal | encoding |
|---|---|---|
| **values** (`value.go`) | be small | varint, zigzag for signed, length-prefixed bytes |
| **keys** (`key.go`) | sort correctly as raw bytes | fixed-width big-endian, sign-flipped |

Conflating these is a common mistake, and it is not recoverable later: an index
built on a non-order-preserving key encoding returns wrong answers for every
range query.

**Length prefixing** solves the boundary problem — write the length, then the
bytes:

```
08 73 63 75 74 65 2D 64 62 05 68 65 6C 6C 6F
^^ 8 bytes follow         ^^ 5 bytes follow
```

**Varints** cost 1 byte for values under 128 and grow to 10 for the largest
`uint64`, versus a flat 8 for fixed-width. Signed values use zigzag first, so
`-1` costs 1 byte rather than 10. Verified byte-identical to `encoding/binary`.

**Keys use big-endian** because the B+Tree will compare them with
`bytes.Compare` and nothing else. Sorting `[1 2 255 256 300]` by raw bytes:

```
big-endian     [1 2 255 256 300]   correct
little-endian  [256 1 2 300 255]   wrong
varint         [1 2 256 300 255]   wrong
```

**Signed keys flip the top bit.** In two's complement `-1` is all `FF` bytes and
sorts above `1`. XOR-ing the sign bit shifts the signed range onto the unsigned
range, preserving order:

```
value   raw two's complement      key encoding
-1      FF FF FF FF FF FF FF FF   7F FF FF FF FF FF FF FF
 0      00 00 00 00 00 00 00 00   80 00 00 00 00 00 00 00
 1      00 00 00 00 00 00 00 01   80 00 00 00 00 00 00 01
```

Floats use the same idea: flip the sign bit if positive, flip every bit if
negative.

**Round-trip property tests.** Table tests cover the specific cases; four Go
fuzz targets cover the rest — value round-tripping, integer and float key order
preservation (`a < b` must imply `bytes.Compare < 0`, for every pair the fuzzer
can find), and that no decoder panics on arbitrary input. Roughly 430–480k
executions/sec.

Two correctness details the float fuzzer exists to protect:

- **`-0.0` is normalised to `+0.0` before encoding.** IEEE-754 says they are
  equal, but their bit patterns differ maximally, so without normalisation an
  index would place them at opposite ends and `WHERE x = 0.0` would miss rows
  stored as `-0.0`.
- **All NaN payloads canonicalise to one.** NaN has many bit patterns; without
  this, two NaNs would be different index keys. They sort above every real
  number, matching Postgres.

**`Bytes` copies, `BytesRef` does not.** `BytesRef` returns a view into the
caller's buffer for hot paths; `Bytes` copies. The distinction matters because
page buffers are recycled by the buffer pool, so a view outlives its backing
bytes. The name is the only warning, so the split is deliberate rather than a
single ambiguous function.

Run it: `make demo-encode`.

### `0x04` Nulls

A byte of zeros in a page is ambiguous: it may be the number `0`, or a field
that was never given a value. Those mean different things and must be
distinguishable.

Sentinel values (`-1`, `0`, `MinInt64`, `""`) do not work, because every
sentinel removes a legal value from the type's range. MySQL's `0000-00-00` date
and the `-1` "no sensor reading" convention are the same mistake in production.

`internal/nullbits` puts the answer outside the data instead: a **bitmap in the
record header**, one bit per field.

```
field    value      stored?
id       42         yes
name     suhail     yes
age      NULL       NO - 0 bytes
email    NULL       NO - 0 bytes
score    9.1        yes

bitmap:   ..NN.---   (N null, . present, - padding)
record:   0C 54 06 73 75 68 61 69 6C 40 22 33 33 33 33 33 33
          ^^ header, then only the three present values
```

Two consequences worth stating explicitly:

- **A null field occupies zero bytes.** The bitmap is not a description of the
  data, it is the only record that the field exists at all.
- **The bitmap must precede the values.** A reader cannot parse the value area
  without first knowing which fields were skipped. This is why it lives in the
  header, and it is why Postgres puts `t_bits` in its tuple header.

One bit per field rather than one byte: a 32-column row spends 4 bytes instead
of 32.

The bitmap is sized in whole bytes, so a 5-field record has 3 unused trailing
bits. The bitmap does not know the field count — the schema does. `Describe`
renders padding as `-` so this is visible rather than misleading.

**Three-valued logic.** SQL's `NULL` means *unknown*, not *empty*, which makes
comparison return `UNKNOWN` rather than true or false. `internal/nullbits`
implements `Bool3` with the SQL truth tables. The consequence:

```
age is NULL

age = 30                  -> UNKNOWN
age != 30                 -> UNKNOWN
age = 30 OR age != 30     -> UNKNOWN
WHERE keeps the row?      -> false
```

A condition that holds for every number that exists still excludes the row,
because `WHERE` keeps `TRUE` only. That is why SQL needs `IS NULL` as separate
syntax: `= NULL` can never be true.

Unknown does not always propagate — `FALSE AND UNKNOWN` is `FALSE`, because the
answer is already decided. The truth tables are pinned by test.

Two API details that fall out of getting this right:

- `Count(fields)` and `Any(fields)` take the field count rather than reading the
  whole bitmap. A bitmap is sized in whole bytes, so a corrupt or stray padding
  bit would otherwise be reported as a phantom null field.
- `Bool3` normalises out-of-range values to `UNKNOWN`, so a garbage value cannot
  make `AND` return `TRUE`.

Run it: `make demo-nulls`.

### `0x05` Alignment and padding

`internal/slots` lays fixed-size records out inside a page so that any slot is
reachable by arithmetic rather than by scanning.

```
slot     offset       how it is found
0        16           16 + 0 x 16
1        32           16 + 1 x 16
199      3200         16 + 199 x 16
```

Measured against packed, length-prefixed records in the same page:

```
fixed slot, first          4-6 ns
fixed slot, 200th          4-5 ns     flat
packed record, first       6-7 ns
packed record, 200th       ~990 ns    about 200x slower
```

The fixed layout does not care which slot you ask for. The packed layout has to
decode every record before the one it wants.

Ranges rather than single figures on purpose: across three runs this
microbenchmark varies by about 1.7 ns, so the fixed-slot first and 200th figures
overlap. That overlap *is* the result — the cost does not depend on the index.
The packed 200th figure is stable to within 4 ns, because 990 ns of real work
drowns out the noise.

**Alignment** is why slot sizes are rounded up to a multiple of 8. Memory is
fetched in words, not bytes, so an 8-byte integer starting at offset 6 straddles
two words and costs two fetches. The same shape appears at page scale: a record
straddling two 4096-byte pages costs two page reads. Rounding up buys
single-fetch access; the rounding is the padding.

Go's compiler already does this to every struct:

```
badOrder    bool, int64, bool, int64    32 bytes
goodOrder   int64, int64, bool, bool    24 bytes
```

Same four fields, 8 bytes saved by ordering them largest-first.

**The cost is real and worth seeing:**

```
record  slot  pad  per page  wasted  waste %
1       8     7    510       3570    87.2%
8       8     0    510       0        0.0%
13      16    3    255       765     18.7%
16      16    0    255       0        0.0%
17      24    7    170       1190    29.1%
```

Sizes already a multiple of 8 waste nothing. A 1-byte record padded to 8 throws
away 87% of the page — small fixed records are where this design stops paying.

**The trade:** fixed slots spend bytes to buy constant-time access; packed
records spend time to save bytes. Rows of varying length cannot use fixed slots
at all, which is why the heap file will need a slot directory instead.

Run it: `make demo-align`, `make bench`.

### `0x06` B+Tree in memory

`internal/btree` is the first structure that makes "find this key" fast. No disk
yet, so the algorithm is the only thing being debugged.

**Why a tree and not something simpler**, measured over 100,000 keys:

```
structure     find one key   range query   problem
linear scan   ~60,000 ns     yes           reads everything
hash map      5-10 ns        NO            no order at all
B+Tree        55-60 ns       yes           -
```

Ranges across three runs. A hash map is roughly **8x faster than the B+Tree** at
finding one key, and completely useless for `WHERE id BETWEEN 200 AND 300`. That
single column is why databases index with trees rather than hash tables: the
B+Tree is not the fastest way to find one key, it is the fastest way to find one
key *while keeping the ability to walk in order*.

**Internal nodes hold only separators; every value lives in a leaf.** This is
the difference between a B-Tree and a B+Tree, and it is what will make range
scans cheap once leaves are chained together.

```
internal [070]
    internal [030 050]
        leaf     [010 020]
        leaf     [030 040]
        leaf     [050 060]
    internal [090 110]
        leaf     [070 080]
        leaf     [090 100]
        leaf     [110 120]
```

**Splitting** happens when a node exceeds `order - 1` keys, and the two cases
differ in a way that is easy to get wrong:

- a **leaf** split *copies* its middle key upward — the key still has a value,
  so it must stay in the leaf as well
- an **internal** split *moves* its middle key upward — it is only a separator,
  so keeping a copy would be duplication

When the root itself splits, a new root is created above it. That is the only
way the tree gets taller, which is why **all leaves stay at the same depth**
automatically.

**Why three levels is enough:**

```
order   level 2      level 3        level 4
64      4,032        258,048        16,515,072
256     65,280       16,711,680     4,278,190,080
512     261,632      133,955,584    68,585,259,008
```

At order 512 a four-level tree addresses over 68 billion keys. Height grows like
log(n), so multiplying the data by 500 adds **one** level. On disk each level is
one page read, so that is 4 reads rather than 4 billion.

Measured: 100,000 keys built in 18ms at order 64, height 4, any key found in
about 55 ns. Inserting costs ~110 ns, roughly twice a lookup, which is the cost
of shifting keys within a node.

The zero value is usable: an unconfigured `Tree` reads as empty and initialises
itself at `DefaultOrder` (64) on first write, rather than panicking.

**Invariants**, checked by `Validate()` after every insert in the tests and in
two fuzz targets:

- keys sorted within every node
- every key inside its subtree's bounds
- all leaves at the same depth
- `len(children) == len(keys) + 1` in internal nodes
- no node over `order - 1` keys, no non-root node under `(order-1)/2`
- the root is a leaf or has at least two children

**The trap the tree cannot save you from:** it sorts with `bytes.Compare` and
nothing else, so a wrong key encoding gives wrong answers silently.

```
sorted by raw bytes (codec.AppendKeyUint64):  [20 30 100 110 120]
sorted as text:                               [100 110 120 20 30]
```

That is exactly what `0x03` was for.

Run it: `make demo-btree`.

### `0x07` Range scans and the iterator

Leaves are now chained left to right, and `Scan(from, to)` returns a cursor that
descends once and then walks sideways.

```
internal [070]
    internal [030 050]
        leaf     [010 020] ──┐
        leaf     [030 040] ──┤  chained in order
        leaf     [050 060] ──┘
```

**Ranges are half-open, `[from, to)`** — `from` included, `to` excluded, a `nil`
bound unbounded. Half-open means ranges join cleanly: `[0,10)` then `[10,20)`
covers everything once, with no gap and no overlap.

**Why the chain is worth it.** Node visits for a range, at order 64 over 100,000
keys:

```
range     Scan (chained)   Get in a loop   ratio
10        4                40              10x
100       7                400             57x
1,000     35               4,000           114x
10,000    316              40,000          127x
```

`Scan` pays for one descent and then walks; calling `Get` per key pays a full
descent every time. Once the tree is on disk a node visit is a page read, so
that is 316 reads against 40,000.

**Lazy evaluation** is the other half. Rows are produced one at a time, so work
not asked for is never done:

```
first 10        (10 rows)       200ns
first 1,000     (1,000 rows)    7.4µs
first 100,000   (100,000 rows)  758.8µs
```

Ten rows out of a hundred thousand costs almost nothing, because the other
99,990 are never touched. That is what `LIMIT` rides on.

**Streaming rather than materialising:**

```
                          time      memory
streaming (iterator)      521µs     80 bytes, 1 allocation
collecting into a slice   8.4ms     18.5 MB, 100,030 allocations
```

Both walk the same 100,000 rows. The iterator holds a position, not the rows —
which is why a database can return a billion-row result to a client that only
wants the first page.

**Why a B+Tree beats a B-Tree here.** A B-Tree keeps values in internal nodes
too, so an in-order scan has to climb between levels and its reads land all over
the file. A B+Tree keeps every value in a leaf and chains the leaves, so a scan
touches one level, in order — turning random reads into sequential ones.

**A new invariant.** `Validate()` now checks the leaf chain visits every leaf
exactly once, in tree order, and terminates. A split that forgets to link
produces a scan that silently returns partial results, so there is a negative
test proving `Validate` rejects both a broken chain and a cyclic one.

`Scan` is verified against a brute-force filter over 300 random ranges, and by a
fuzz target that ran 2.9M cases comparing scan output to the same brute force.

Run it: `make demo-range`.

### `0x08` B+Tree deletion

The step most from-scratch databases stop at. Insert has one failure mode and
one answer; delete has one failure mode and **three**, any of which can cascade.

```
insert                     delete
does it fit? done          still enough keys? done
too full -> split          too empty -> borrow from left
                                     or borrow from right
                                     or merge
                           and the merge may cascade up
                           and the root may collapse
```

Every one of those six behaves differently for a leaf than for an internal node,
which is where the "3x harder" comes from.

**All four repair paths in one trace** at order 4 (max 3 keys, min 1) — the demo
reports what actually happened rather than what was expected:

```
delete 030 -> fitted, no repair needed
delete 040 -> BORROWED from the LEFT sibling
delete 010 -> MERGED two nodes into one
delete 020 -> BORROWED from the RIGHT sibling
delete 050 -> MERGED two nodes into one
delete 060 -> BORROWED from the RIGHT sibling
delete 070 -> MERGED two nodes into one, then the ROOT COLLAPSED
```

**Borrowing rewrites one separator. Merging deletes one** — which is what can
make the parent underflow in turn, and so on up. The tree grew from the root and
it shrinks back to the root:

```
2000 keys      -> height 7
delete 1990    -> height 3
along the way: 1487 borrows, 1485 merges, 4 root collapses
```

**The leaf/internal asymmetry mirrors the split asymmetry exactly.** A leaf
*split* copies its middle key up; an internal split *moves* it. A leaf *merge*
discards the separator; an internal merge *pulls it down* between the two halves,
because for an internal node that separator is the only copy of the key.

**Ghost separators.** A separator only has to point a search the right way — it
does **not** have to name a key that exists. Deleting every key that appears as a
separator, then re-checking:

```
deleted all 49 keys that appeared as separators
49 of the 49 separators left now name keys that are gone
lookups that return the wrong answer: 0 of 100
validate: <nil>
```

This is why deletion never has to hunt upward and repair separators — a large
amount of work the algorithm gets to skip, and the reason a stale separator is
correct rather than a bug.

**Why some engines never merge at all.** Merging keeps the index dense:

```
                     leaves   keys   fill
4000 keys            500      4000   53%
after deleting half  249      2000   54%
```

251 leaves handed back, fill unchanged. Without merging the leaf count would
have stayed at 500 and the fill halved — the index growing while the data
shrinks. And yet **Postgres's B-Tree does not merge partially-empty pages**; it
only reclaims entirely empty ones. That is exactly why index bloat is an
operational concern there and why `REINDEX` exists. Merging costs write
amplification and page locks on a hot path, and some engines judge that trade
not worth making.

**`index.Index` is now satisfied.** With `Delete` in place, `btree.Index` wraps
the tree to match the interface written in `0x00` — asserted by
`var _ index.Index = (*Index)(nil)`. The natural API stays natural (`Get`
returns a bool, `Put` cannot fail in memory); the adapter translates.

**How it is verified.** A reference `map` is maintained alongside the tree
through 4,000 random insert/delete operations at five orders, with `Validate()`
after **every** operation and an exhaustive `Get` check over the whole key space
at the end. `TestMinKeysHoldsForEveryOrder` inserts 400 and deletes 400 for
every order from 3 to 40. `FuzzInsertDeleteAgainstReference` ran 344,719
executions, each up to 3,000 operations, validating after each one.

Run it: `make demo-delete`.

### Known gaps at the end of Phase 0

Deliberate, each one is a later step:

- ~~`Page.Append` writes items with no separator~~ → fixed in `0x03`
- No way to find item *n* without walking items 1..*n-1* → `0x0F` (slot directory)
- `File.Allocate` never reuses a freed page → `0x0A` (free list)
- The reserved header bytes hold no checksum → `0x13`
- Nothing is thread-safe → `0x0C`

---

## Layout

```
cmd/scutedb-demo/     the runnable experiments
internal/
  core/             PageID, RowID, shared errors
  fileio/           File interface + OSFile
  index/            Index interface
  storage/          Engine interface
  naive/            the throwaway from 0x01
  page/             fixed-size pages, header codec, header decoder
  codec/            value encoding (compact) and key encoding (ordered)
  nullbits/         null bitmaps and SQL three-valued logic
  slots/            fixed-size, aligned record slots inside a page
  btree/            in-memory B+Tree: search, insert, split, range scans, delete
```

## Conventions

- Go 1.22+, standard library only. No dependencies, on purpose.
- `gofmt` and `go vet` clean before every commit.
- Comments are omitted; the reasoning lives here and in commit messages.
- Every step ships with a test, and where bytes are involved, a hexdump.

## License

MIT — see [LICENSE](LICENSE).

Copyright (c) 2026 Syed Suhail Ahmed.

`ScuteDB` has no third-party dependencies, so there are no license-compatibility
constraints to check.
