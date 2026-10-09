# ScuteDB

A database engine written from scratch in Go, one layer at a time.

A *scute* is one of the bony plates that make up a turtle's shell. A shell is
made of plates; a database file is made of pages. 


**Status:** Phases 0 and A complete. Phase B is under way: a B+Tree node has a
byte-for-byte on-disk format, and a storage manager allocates pages, recycles
them, and finds the root again after a restart — including after a `kill -9`.

---

## Quick start

```
go test ./...              # everything
make demo                  # list the runnable experiments
make demo-scan             # watch a file-based database degrade
make hexdump               # write real pages and look at the bytes
make demo-delete           # borrow, merge and root collapse, traced live
make demo-nodepage         # decode a real B+Tree page out of a real file
make demo-pagestore        # free lists, checkpoints, and twenty kill -9 restarts
```

---

## Roadmap I am trying to follow plan first execute second

| Phase | What | Status |
|-------|------|--------|
| **0** | Foundations — interfaces, the naive database, pages | **done** |
| **A** | Bytes & the B+Tree | **done** |
| B | Persistence — storage manager, buffer pool, locking | **in progress** |
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

### Phase A in one paragraph

Six steps turned raw bytes into a working index. `codec` ships two encodings
because keys and values have different jobs — values optimise for size, keys
optimise for sorting correctly as raw bytes, and conflating them is not
recoverable later. `nullbits` makes "no value" a property of the row rather than
a magic value hidden inside it, and carries the three-valued logic that follows
from that. `slots` buys O(1) addressing inside a page for a few bytes of padding.
`btree` turns a linear scan into three or four hops, keeps the leaves chained so
a range costs one descent and then a sideways walk, and now repairs itself when
deletes empty a node out.

What is still missing is the part that makes this a database rather than a data
structure: none of it survives a restart. Every node is a Go pointer. Phase B
replaces those pointers with page IDs, and the tree starts living on disk.

### Known gaps at the end of Phase A

Deliberate, each one is a later step:

- ~~`Page.Append` writes items with no separator~~ → fixed in `0x03`
- The B+Tree is pointers in memory and nothing persists → `0x09` (nodes become pages)
- No way to find item *n* without walking items 1..*n-1* → `0x0F` (slot directory)
- ~~`File.Allocate` never reuses a freed page~~ → fixed in `0x0A` (free list)
- The reserved header bytes hold no checksum → `0x13`
- Nothing is thread-safe → `0x0C`

---

## Phase B — Persistence

### `0x09` Nodes as pages

Every node in `0x06` to `0x08` was a Go struct held together by pointers. A
pointer is a RAM address: it is meaningless in the next process, so the entire
tree evaporates on exit. This step gives a node a byte layout and swaps every
pointer for a **page number**.

```
in memory                          on disk
children []*node   8 bytes each    child core.PageID   4 bytes each
                   a RAM address                       a page number
                   valid until exit                    valid forever
```

Page 12 is at byte `12 * 4096 = 49152` — in this process, in the next one, and on
another machine. That single swap is the whole step; everything below follows
from it.

**The layout.** `internal/nodepage` is a *slotted page*, the same shape Postgres
and SQLite use:

```
region         bytes         holds
page header    0..15         id, kind, key count, free start, free end
node header    16..23        next leaf / first child, level, 2 pad
slot array     24..          4 bytes per entry, in KEY order
free space                   shrinks from both ends
cells          ..4095        key + payload, in INSERTION order
```

Slots grow up from byte 24, cells grow down from byte 4095, and free space is
whatever is left between them. The trick worth noticing: **the slot array is
sorted, the cells are not.** A slot is 4 bytes (`offset`, `length`) and inserting
a key in the middle means shifting a few 4-byte slots, never the key bytes
themselves. Binary search reads the slot array, so lookups stay `O(log n)` while
writes stay cheap.

**Two cell shapes**, and the key length never needs storing because the suffix is
fixed:

```
leaf cell      key bytes || row page (4) || row slot (2)      key = len - 6
internal cell  key bytes || child page  (4)                   key = len - 4
```

An internal node with *n* keys has *n+1* children. The extra child has no slot to
live in, so it lives in the node header — that is what `first child` is.

**Fanout is computed, not chosen.** With a 4096-byte page and an 8-byte key:

```
  page header        16
  node header         8
  left for entries 4072

  internal entry = 4 slot + 8 key + 4 child  = 16 bytes -> 254 keys, 255 children
  leaf entry     = 4 slot + 8 key + 6 row id = 18 bytes -> 226 keys

  levels   reads per get  keys it holds
  1        1              226
  2        2              57,630
  3        3              14,695,650
  4        4              3,747,390,750
```

This corrects a number stated earlier in this file. `0x06` estimated ~340 keys
per internal node from `4080 / (8 + 4)`, which quietly ignored the node header
and the 4-byte slot every entry needs. The real figure is **254** — the
bookkeeping costs 25% of the fanout. The estimate was optimistic in the usual
direction: it counted the data and forgot the structure that makes the data
findable.

**A real page, out of a real file.** Three pages written to disk, then read back
with `xxd` — no ScuteDB code involved in the reading:

```
$ xxd -s 4096 -l 32 tree.db
00001000: 0000 0001 0300 0002 0020 0fe4 0000 0000  ......... ......
00001010: 0000 0002 0000 0000 0ff2 000e 0fe4 000e  ................
```

Decoding it by hand, left to right:

| bytes | value | meaning |
|---|---|---|
| `0000 0001` | 1 | page id |
| `03` | 3 | kind, `btree-leaf` |
| `00` | 0 | flags |
| `0002` | 2 | key count |
| `0020` | 32 | free start, `24 + 2 slots x 4` |
| `0fe4` | 4068 | free end, where the lowest cell begins |
| `0000 0000` | — | reserved for the checksum in `0x13` |
| `0000 0002` | 2 | next leaf is page 2 |
| `0000` | 0 | level, 0 means leaf |
| `0000` | — | padding, keeps slots 4-byte aligned |
| `0ff2 000e` | 4082, 14 | slot 0 points at a 14-byte cell |
| `0fe4 000e` | 4068, 14 | slot 1 points at a 14-byte cell |

Slot 0 says byte 4082 of page 1, which is file offset `4096 + 4082 = 8178`:

```
$ xxd -s 8178 -l 14 tree.db
00001ff2: 7fff ffff ffff ffff 0000 0064 0000       ...........d..
```

`7F FF FF FF FF FF FF FF` is the key **-1**, sign bit flipped by the ordered
encoding from `0x03`. Then `0000 0064` is row page 100, and `0000` is slot 0.
Every layer built so far is visible in those fourteen bytes.

**Walking the tree with no pointers at all**, reading pages back from the file:

```
looking up 30, starting at page 0
  read page 0 at byte offset 0, kind btree-internal, 1 keys
  not a leaf, so follow a page id: next is page 2
  read page 2 at byte offset 8192, kind btree-leaf, 2 keys
  found at slot 1 -> row id page 200 slot 30
```

**How the real ones do it.** Postgres calls a page number a `BlockNumber`, a
`uint32` index into the relation file, and its B-Tree pages carry a `btpo_level`
exactly like the level field here. SQLite uses 1-based 32-bit page numbers, with
page 1 always holding the schema. InnoDB uses 32-bit page numbers inside a
tablespace with 16 KB pages. Nobody stores an address.

**How it is verified.** `Validate` checks twelve structural facts, including that
the cells exactly tile the region from `free end` to the end of the page with no
gaps and no overlaps, and that the slot array is in strictly ascending key order.
Eleven separate corruptions are asserted to fail it. `TestGoldenLeafLayout`
pins fourteen exact byte ranges so the format cannot drift silently, and
`TestMaxKeysMatchesWhatActuallyFits` fills a page for every key length from 1 to
64 and checks the count against the arithmetic. A fuzz target built pages from
arbitrary keys for 2.38M executions.

Run it: `make demo-nodepage`.

### `0x0A` The index storage manager

`0x09` made a tree node into bytes. It left one question open, and it is the
first thing any reader of the file asks: **where does the tree start?** After a
restart nothing is in memory. A file holding 14 million perfectly formatted keys
is useless if nothing says which page is the root.

`internal/pagestore` answers it, and takes on everything that comes with owning
a file: handing pages out, taking them back, growing the file, and making sure a
crash at any moment leaves something that still makes sense.

**The meta page is the one fixed address.** Page 0 is the only place a reader can
look without being told where to look, so it holds everything needed to find
everything else:

```
page 0, the meta page

0000  00 00 00 00 01 00 00 00  00 38 10 00 00 00 00 00  |.........8......|
0010  53 43 55 54 45 44 42 00  00 00 00 01 00 00 10 00  |SCUTEDB.........|
0020  00 00 00 01 00 00 00 00  00 00 00 00 00 00 00 02  |................|
0030  00 00 00 10 00 00 00 02  00 00 00 00 00 00 00 00  |................|

offset    bytes                      meaning
16..23    53 43 55 54 45 44 42 00    magic = "SCUTEDB", NUL-terminated
24..27    00 00 00 01                format version = 1
28..31    00 00 10 00                page size = 4096
32..35    00 00 00 01                root = page 1
36..39    00 00 00 00                free list = none (nothing is free)
40..43    00 00 00 00                free page count = 0
44..47    00 00 00 02                next page to hand out = 2
48..51    00 00 00 10                pages the file holds = 16 (65536 bytes)
52..55    00 00 00 02                checkpoint generation = 2
```

Page 0 doubles as the "no page" value — `root = 0` means no root yet, and a
free-list chain ends at 0. That is the trick `0x04` warned against, and here it is
safe for exactly the reason it was unsafe there: a sentinel is only dangerous when
the value it steals could be real. `-1` can be a real integer. Page 0 can never be
a root or a free page, because it is always this page.

**Magic numbers and versions exist to refuse.**

```
open a text file          -> rejected, not a scutedb file
open a version-2 file     -> rejected, unknown format version
open 8192-byte pages      -> rejected, wrong page size
```

None of those would crash if accepted. Reading 8 KB pages as 4 KB pages would
quietly return wrong rows, which is worse. Two rules make the refusal *useful*:

- **The magic never carries the version.** An earlier draft ended the magic with
  a `01` byte, so a version-2 file would have been called foreign rather than
  newer. The magic answers "is this ours?"; the version field answers "which
  one?". Keeping them apart is what lets a reader say *this is a ScuteDB file,
  newer than me* instead of *this is not a ScuteDB file*.
- **Check identity, then version, then everything else.** Everything after the
  version is allowed to change between versions — including the page header — so
  a newer file must be recognised before any of it is read.

SQLite's first sixteen bytes are `SQLite format 3` followed by a NUL, for the same
reason.

**The file grows in chunks.** Pages handed out and pages the file holds are two
different numbers, so the meta page stores both:

```
handed out   file size        grown
0            64.0 KB          1 times
15           64.0 KB          1 times
16           128.0 KB         2 times
31           128.0 KB         2 times
32           192.0 KB         3 times
40           192.0 KB         3 times
```

Forty pages, three growths, one write each.

**Free lists, and why a freed page has to wait.** The first draft of this step did
what most tutorials do: freeing a page wrote a "free" marker and a next-pointer
into the page itself, so the free list was a chain threaded through the free
pages. Two crash tests broke it:

- **Data loss.** Checkpoint a root, make a new root, free the old one — exactly
  what a B+Tree root collapse does — then crash before the next checkpoint. On
  restart the meta page correctly points at the old root, but `Free` had already
  overwritten it. The last committed tree was destroyed by work that was never
  committed.
- **A corrupt free list.** Reusing a free page overwrote its link, so after a
  crash the durable free list walked into a leaf.

One cause behind both: **overwriting a page the last durable state still points
at.** Until a new checkpoint is durable, nothing the previous one references may
be touched. The fix is the design LMDB and BoltDB use:

```
Free(page)       goes on a PENDING list, bytes untouched
                 not reusable until the checkpoint that stops referencing it

checkpoint       pending pages join the free list
                 the free list is written into its OWN pages, taken only from
                 pages already free in the durable state — never from pending
                 pages, never from the current free-list pages
```

```
free pages 3, 4 and 5
  reusable now               none
  pending, after checkpoint  3 4 5

allocate -> page 7. a NEW page, not one just freed.

checkpoint
  reusable now               3 4 5
  holding the free list      8

allocate -> page 5. reused, and the file did not grow.
```

The old free-list pages are listed as free in the new list, so they are recycled
rather than leaked — two hundred allocate-free-checkpoint rounds keep the file at
one chunk.

**Data durability is not structural durability.** Data durability means a page's
bytes reached the disk. Structural durability means the file *as a whole*
describes a state that makes sense. A checkpoint turns the first into the second,
and the order is the whole algorithm:

```
1. fsync            every page written since the last checkpoint
2. write            the new free list into its own pages
3. fsync            so the list is on disk
4. write page 0     the new root, list, and page counts
5. fsync            the commit point
```

The meta page is written last. A crash anywhere before step 5 leaves page 0
describing the previous checkpoint, with nothing it points at disturbed. That
relies on a 56-byte write being atomic, which holds because it sits inside one
disk sector; Postgres makes the same bet with `pg_control`, which it keeps under
512 bytes for exactly this reason. On macOS a plain `fsync` does not flush the
drive's own write cache, but Go's `File.Sync` already issues `F_FULLFSYNC` there
(falling back to `fsync` where a filesystem cannot do it), so these really do
reach the storage.

**The page cache is not the disk.** Most of what went wrong in this step came from
one confusion. A read returns what the *operating system* holds, which is not
necessarily what the *disk* holds. Three separate bugs trusted the first as if it
were the second:

| trusted as durable | what could really be on disk | fix |
|---|---|---|
| the meta page `Open` just read | an older meta page | **recovery-on-open**: rewrite the meta and fsync it before handing out a single page |
| the file size | a shorter file | growth writes zeros for exactly the new pages, from the store's own count, never from the file's reported size |
| the file's name in its directory | no directory entry at all | `Open` and `Create` fsync the directory that *really* holds the file, following symlinks, every time |

The first is the subtle one. If a checkpoint's final fsync fails, or the process
dies during it, the new meta page sits in the cache but not on disk. The next
`Open` would believe it and start reusing pages the *on-disk* checkpoint still
needs; a later power cut then reverts to that older checkpoint, now overwritten.
Rewriting the meta on open makes what the store believes and what the disk holds
agree before anything else happens. SQLite does the equivalent when it rolls back
a hot journal on open.

The directory row took three tries, and the way it went wrong twice is the lesson.
The first fix fsynced `filepath.Dir(path)` — the directory named in the path's
*text*. Through a symlink the kernel puts the file's entry in the *target's*
directory, so the fsync hit the wrong one. The second fix resolved the path with
`filepath.EvalSymlinks` and fsynced that. It was right about where the file is,
but it built the whole resolved path as one string, which fails once it passes
`PATH_MAX` even though the kernel reaches the same file one step at a time. And it
also fsynced a "lexical parent" computed with `filepath.Dir`, which tidies away
`..` before following symlinks and can name a directory that does not exist. Both
made valid files unopenable.

The third fix stops resolving paths in strings at all. It splits off the last
component without cleaning anything, opens that prefix as a directory, and lets
the kernel resolve every symlink and `..` along the way. The only thing it follows
itself is a symlink in the *final* component, using `Lstat` and `Readlink` and
joining the target onto the raw prefix.

**A missing database is an error, not an empty database.** `Open` used to create
the file if it did not exist. That is what turned every lost directory entry into
silent data loss: after a power cut dropped the file's name, the next `Open`
quietly handed back a fresh, empty store. Now only `Create` makes files, and
`Open` of a missing path fails with `fs.ErrNotExist`. `Open` still initialises a
file that exists but is blank, because that is what an interrupted `Create` leaves
behind.

**When fsync fails, stop.** Any failed fsync poisons the store; every later call
returns `ErrPoisoned`, carrying the original error, until the file is reopened.
In 2018 Postgres found that Linux could drop dirty pages on a failed fsync, mark
them clean, and report success on the retry — so a retry can "succeed" having
written nothing. Postgres now panics instead. Errors that happen before anything
durable is touched, such as running out of page ids, return plainly and leave the
store usable.

One consequence worth stating plainly: **after a failed checkpoint, the outcome
is unknown.** If only the final fsync failed, the new meta may already be in the
cache, and the next `Open` will make it durable. The caller has to reopen and read
the root to find out — the same position as a Postgres client whose `COMMIT` timed
out.

**Restart as a test.** The demo runs a child process that loops — new root, stamp
it with the checkpoint number it is about to produce, free the old root,
checkpoint — and `kill -9`s it at an arbitrary moment, twenty times:

```
run   child said   file says    root stamp verify
1     4            5            5          ok
2     6            6            6          ok
3     6            7            7          ok
...
19    26           27           27         ok
20    27           27           27         ok

20 of 20 recovered to a consistent checkpoint.
```

"File says" is sometimes one ahead: the child finished a checkpoint and died
before it could print. What must never happen is the file being *behind*.

The same thing runs as an ordinary test, `TestARealKillNineAtAnyMomentAlwaysRecovers`.
The test binary starts a copy of itself as the child, kills it with a real
`kill -9` twelve times at different moments, and checks each file. So it runs in
CI on Linux with every pull request, not only when someone runs the demo.

A `kill -9` only kills the process; the OS still flushes what it was handed. The
harder cases cannot be staged from a demo, so the tests use a simulated disk that
models an operating system in front of a drive:

- a **power cut** keeps the last synced image plus any chosen subset of the writes
  since;
- a **failed fsync** behaves like Linux after 2018 — some writes land, the rest are
  marked clean and never reach the disk, though reads still see them;
- a **process restart** keeps the page cache, which is exactly where the
  cache-versus-disk bugs live;
- **eviction** lets those clean-but-unwritten pages silently revert to the older
  bytes on disk.

`FuzzCrashAnywhereKeepsTheLastCheckpoint` runs random sequences of allocate, free,
set-root, checkpoint, clean restart, process crash, power cut, failed checkpoint
and failed sync, and after every single step checks that every page the last
checkpoint referenced still holds its original bytes, and that every page ever
handed out is in exactly one state. It ran 2,436,886 executions of up to 400
operations each.

**What independent review found.** Four reviewers, each working in a private copy
of the repository, attacked the first version from different angles — crash
consistency, page accounting, the file format, and misuse of the API. Every
finding had to come with a failing test, and a separate skeptic had to reproduce
it before it counted. A second round attacked the fixes; one of its reviewers
wrote an independent crash fuzzer from scratch, with a different definition of
"correct", and it found nothing. A third round attacked what the second round
changed, and a fourth attacked what the third changed — finding that the third
round's own fix had introduced two ways to make a valid file unopenable.

| bug | severity | found by |
|---|---|---|
| `Free` overwrote pages the last checkpoint still referenced | data loss | crash tests, first draft |
| `Open` trusted a meta page that existed only in the cache | data loss | review, round 1 |
| a failed `Sync` did not poison, so the next checkpoint committed lost pages | data loss | review, round 1 |
| page ids could wrap past 2³² into the meta page | data loss | review, round 1 |
| a root that was also a free-list page was accepted, and later handed out | corruption | review, round 1 |
| a crash during `Create` left a file that could never be opened | unrecoverable | review, round 1 |
| `Verify` never compared live state with the durable free list | blind spot | review, round 1 |
| growth trusted the file size the cache reported | corruption | **the fuzzer, while testing another fix** |
| the directory was fsynced only when `Open` created the file | data loss | review, round 2 |
| running out of page ids reported "an fsync failed" and hid the cause | wrong error | review, round 2 |
| the directory fsync followed the path's text, not its symlink | data loss | review, round 3 |
| that fix built the resolved path as one string, which fails past `PATH_MAX` | unopenable | review, round 4 |
| that fix also tidied away `..` before following symlinks | unopenable | review, round 4 |
| a failed `Allocate` that had grown the file left the store reporting the bigger size | wrong state | fault-injection tests |
| a failed `Write` still marked the store as needing a checkpoint | wrong state | fault-injection tests |

**How strong are the tests?** A test suite can pass and still miss bugs. Two
measures say how much it would really catch.

*Coverage* asks which lines run during the tests. It is the weaker measure: a
line can run without any test checking its result.

*Mutation testing* is the stronger one. A script breaks the code on purpose —
flips a `<` to `<=`, switches off an `if`, makes an error return `nil`, deletes
a line that updates state — one change at a time, 563 changes in all. For each
broken copy it runs the whole suite. If no test fails, that break "survived",
which means the tests would not have noticed that bug.

Every fix above was also broken on purpose by hand, and all 28 were caught. That
number turned out to be flattering: those were the exact spots I had just fixed.
The full sweep told a different story:

```
                         before hardening    after hardening
coverage, storage code   79.7%               93.5%
mutation score           68.9%               93.1%
breaks nobody caught     174                 39
tests                    181                 238
```

The 174 surviving breaks pointed straight at what was missing, and that drove a
round of new tests:

- **Hostile files.** 81 ways of damaging a real store's bytes on disk, each of
  which `Open` must reject with the right error *and without changing the file*,
  plus a fuzzer that fed `Open` random bytes 20.7 million times.
- **Fault injection.** A fake file that fails the Nth write, read, sync or size
  call, or tears a write halfway. It does this at 251 separate points inside
  every operation, and after each one checks that the store either stayed
  exactly as it was or poisoned itself, and that the file still reopens.
- **An independent oracle.** A second crash fuzzer that reads the bytes on the
  simulated disk with its *own* decoder, instead of trusting the store's.
- **Each safety check on its own.** `Open` checks a file twice: once with
  `checkMeta` and `checkFreeSet`, once with `verifyLive`. Break either and the
  other still catches it, so the sweep could never test them separately. Now
  each one is also tested directly.
- **A real `kill -9`** as an ordinary test, described above.

The sweep also caught **weak tests I had written**. Six symlink tests passed if
`Open` failed for *any* reason, not the right one. A `Discard` test passed only
because the test itself called `Close` afterwards. Both now check the exact
cause.

The 39 breaks still surviving were each checked by hand. None is an untested
gap. They fall into three groups:

```
17  no observable difference   e.g. capping at a limit with > or >= gives the same value
15  a second check catches it   two layers guard the same thing; each is tested alone
 7  needs a faked OS failure    e.g. closing a directory failing, which the OS
                                will not do on demand
```

In short: coverage says the code ran; mutation testing says the tests would
notice if it were wrong. The second number went from 69% to 93%.

**How the real ones do it.** SQLite's header is page 1, with a free list of trunk
pages that each list leaf pages. BoltDB keeps two alternating meta pages chosen by
transaction id and checksum, writes its free list to a fresh page on every commit,
and holds freed pages as pending until no reader can still see them. LMDB tracks
free pages in a B-Tree of its own. Postgres keeps its checkpoint location in
`pg_control`.

**Known limits**, each deliberate:

- A single meta page relies on a sector-sized write being atomic. Two alternating
  meta pages with checksums remove that assumption; checksums arrive in `0x13`.
- A database path whose *final* component is a symlink with a relative target
  cannot be opened if that target, joined to the link's directory, is longer than
  `PATH_MAX` (1024 bytes on macOS). Resolving it without building that string
  needs `openat`, which Go's standard library does not expose on macOS.
- A full disk with an empty free list cannot checkpoint, because the new free list
  needs a page nothing durable references.
- A directory fsync is treated as done when the filesystem says it cannot do one
  at all (`EINVAL`, `ENOTSUP`), as SQLite does; on those filesystems a new file's
  name is only as durable as the filesystem makes it. Every other error fails.
- There is no file lock, so two processes can open the same store; that belongs
  with concurrency in `0x0C`. Windows is not supported.
- A checkpoint can occasionally write one empty trailing free-list page; it is
  valid, and recycled at the next checkpoint.
- Nothing is thread-safe yet (`0x0C`), and the B+Tree does not use this store yet.

Run it: `make demo-pagestore`.

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
  nodepage/         the on-disk byte layout of a B+Tree node
  pagestore/        page allocation, free list, meta page, checkpoints
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
