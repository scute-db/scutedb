# ScuteDB

A database engine written from scratch in Go, one layer at a time.

A *scute* is one of the bony plates that make up a turtle's shell. A shell is
made of plates; a database file is made of pages. 


**Status:** Phase 0 complete, Phase A started (4 of 35 steps).

---

## Quick start

```
go test ./...              # everything
make demo                  # list the runnable experiments
make demo-scan             # watch a file-based database degrade
make hexdump               # write real pages and look at the bytes
```

---

## Roadmap

| Phase | What | Steps | Status |
|-------|------|-------|--------|
| **0** | Foundations — interfaces, the naive database, pages | `0x00`–`0x02` | **done** |
| A | Bytes & the B+Tree | `0x03`–`0x08` | **in progress** |
| B | Persistence — storage manager, buffer pool, locking | `0x09`–`0x0D` | |
| C | A real data store — schema, rows, indexes | `0x0E`–`0x12` | |
| D | Transactions — WAL, recovery, 2PL, MVCC | `0x13`–`0x1A` | |
| E | Beyond — LSM engine, Raft, server, query planner | `0x1B`–`0x22` | |

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

**Round-trip property tests.** Table tests cover the specific cases; three Go
fuzz targets cover the rest — value round-tripping, key order preservation
(`a < b` must imply `bytes.Compare < 0`, for every pair the fuzzer can find),
and that no decoder panics on arbitrary input. Roughly 480k executions/sec.

Run it: `make demo-encode`.

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
