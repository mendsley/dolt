# Git Remote Execution Call Tree & Fast-Import Analysis

## Context

This is a research/analysis document exploring how Dolt's git remote operations work internally, mapping the full execution call tree, and analyzing whether `git fast-import` could improve performance of fetch and push operations.

---

## Git Remote Execution Call Tree

### Fetch Call Tree

```
dolt fetch <remote> [<refspec>...]
│
├─ cmd/dolt/commands/fetch.go:74 — FetchCmd.Exec()
│   └─ Constructs SQL: CALL dolt_fetch(...)
│
├─ sqle/dprocedures/dolt_fetch.go:33 — doltFetch()
│   ├─ Resolves remote by name
│   ├─ Parses refspecs
│   └─ Calls env.actions.FetchRefSpecs()
│
├─ env/git_remote_url.go:50 — NormalizeGitRemoteUrl()
│   └─ Detects git remote: git+file/http/https/ssh, .git suffix, scp-style
│
├─ dbfactory/git_remote.go:95 — GitRemoteFactory.CreateDB()
│   ├─ Computes cache path: .dolt/git-remote-cache/<sha256(url|ref)>/repo.git
│   ├─ git init --bare                          ← GIT EXEC #1
│   ├─ git remote add origin <url>              ← GIT EXEC #2
│   ├─ git ls-remote --heads origin             ← GIT EXEC #3
│   └─ Creates nbs.NewGitStore() backed by GitBlobstore
│
├─ nbs/store.go:655 — NewGitStore()
│   └─ Creates GitBlobstore with ref=refs/dolt/data
│
├─ doltdb.go:1854 — Rebase() → syncForRead()
│   ├─ blobstore/git_blobstore.go:527 — syncForRead()
│   │   ├─ git fetch --no-tags --refmap= origin +refs/dolt/data:refs/remotes/origin/data
│   │   │                                       ← GIT EXEC #4 (NETWORK!)
│   │   ├─ git update-ref <localRef> <oid>      ← GIT EXEC #5
│   │   └─ mergeCacheFromHead()
│   │       ├─ git rev-parse --verify <ref>     ← GIT EXEC #6
│   │       └─ git ls-tree -r -t <commit>       ← GIT EXEC #7
│   └─ Updates manifest from fetched state
│
├─ env/actions/remotes.go:493 — FetchRefSpecs()
│   ├─ For each refspec: resolve remote branch HEAD
│   │   └─ Multiple git rev-parse calls          ← GIT EXEC #8..N
│   └─ doltdb.go:1956 — PullChunks()
│       └─ pull/puller.go:66 — NewPuller().Pull()
│           ├─ srcCS.HasMany() — check which chunks exist remotely
│           │   └─ For each chunk: git rev-parse + git cat-file  ← GIT EXEC (per chunk!)
│           ├─ sinkCS.HasMany() — check which chunks exist locally
│           ├─ Fetch missing chunks in batches
│           │   └─ GetManyCompressed() → git cat-file blob <oid>  ← GIT EXEC (per blob!)
│           └─ Write chunks to local NBS table files
│
└─ Update local tracking refs
    └─ FastForward / SetHeadToCommit
```

### Push Call Tree

```
dolt push <remote> [<refspec>...]
│
├─ cmd/dolt/commands/push.go:87 — PushCmd.Exec()
│   └─ Constructs SQL: CALL dolt_push(...)
│
├─ sqle/dprocedures/dolt_push.go:49 — doDoltPush()
│   └─ actions/remotes.go:106 — DoPush()
│       └─ actions/remotes.go:56 — Push()
│           ├─ Validate fast-forward
│           └─ destDB.PullChunks() — transfer chunks TO remote
│
├─ pull/puller.go — Same Puller as fetch, but reversed src/sink
│   ├─ Read chunks from local NBS
│   └─ Write chunks to remote GitBlobstore
│
├─ blobstore/git_blobstore.go:850 — Put() (per chunk)
│   ├─ Non-manifest keys: DEFERRED (no git exec yet!)
│   │   └─ git hash-object -w --stdin           ← GIT EXEC (per blob!)
│   │       Stores blob locally, enqueues pendingWrite
│   └─ Manifest key triggers flush:
│
├─ git_blobstore.go:1090 — CheckAndPut("manifest")
│   ├─ Collects all pendingWrites
│   └─ checkAndPutWithRemoteSync()
│       └─ remoteManagedWrite() with retry loop:
│           ├─ fetchAlignAndMergeForWrite()
│           │   ├─ git fetch (sync with remote)  ← GIT EXEC (NETWORK!)
│           │   ├─ git update-ref                ← GIT EXEC
│           │   └─ mergeCacheFromHead()
│           │       └─ git ls-tree -r -t         ← GIT EXEC
│           ├─ buildCommitForKeyWrite()
│           │   ├─ git read-tree <parent>        ← GIT EXEC
│           │   ├─ For EACH pending write:
│           │   │   └─ git update-index --add --cacheinfo  ← GIT EXEC (per blob!)
│           │   ├─ git write-tree                ← GIT EXEC
│           │   └─ git commit-tree               ← GIT EXEC
│           ├─ git update-ref                    ← GIT EXEC
│           └─ git push --force-with-lease       ← GIT EXEC (NETWORK!)
│
└─ Update local tracking refs
```

### Git Command Summary

| Command | When | Count | Network? |
|---------|------|-------|----------|
| `git init --bare` | Cache setup (once) | 1 | No |
| `git remote add` | Cache setup (once) | 1 | No |
| `git ls-remote --heads` | Cache validation | 1 | Yes |
| `git fetch --no-tags` | Sync remote ref | 1-2 | Yes |
| `git rev-parse` | Resolve refs/paths | Many | No |
| `git ls-tree -r -t` | Cache population | 1-2 | No |
| `git cat-file -t` | Type check | Per object | No |
| `git cat-file -s` | Size check | Per object | No |
| `git cat-file blob` | Read blob | Per chunk read | No |
| `git hash-object -w` | Write blob | Per chunk write | No |
| `git update-index --cacheinfo` | Build index | Per blob in commit | No |
| `git read-tree` | Load parent tree | 1 | No |
| `git write-tree` | Create tree obj | 1 | No |
| `git commit-tree` | Create commit obj | 1 | No |
| `git update-ref` | Update refs | 2-3 | No |
| `git push --force-with-lease` | Push to remote | 1 | Yes |

---

## Performance Bottlenecks

### 1. Per-Object Process Spawning (Critical)
Every `git hash-object`, `git cat-file`, `git update-index` call spawns a new git process. For a push with 1000 chunks, that's:
- ~1000x `git hash-object -w --stdin` (write blobs)
- ~1000x `git update-index --add --cacheinfo` (build index)
- Plus type/size checks during reads

Each process spawn on Windows costs ~10-50ms. 1000 blobs = 10-50 seconds just in process overhead.

### 2. Sequential Index Updates
`buildCommitForKeyWrite()` calls `UpdateIndexCacheInfo()` in a loop — one `git update-index` per blob. This is O(N) process spawns.

### 3. Blob Reads Are Not Batched
`git cat-file blob <oid>` is called per-blob. Git supports `git cat-file --batch` which keeps a single process alive and serves multiple objects through stdin/stdout.

### 4. Small Ranged Reads Are Inefficient
TODO in code (git_blobstore.go:1293): streaming implementation seeks by discarding bytes, no random access.

---

## Fast-Import Analysis

### What is git fast-import?
`git fast-import` is a git plumbing command designed for bulk-importing data into a git repository. It reads a stream of commands on stdin and creates git objects (blobs, trees, commits, tags) in a single process invocation. It's the standard tool for VCS migration and bulk data loading.

### How fast-import could help PUSH

**Current write path (per push):**
```
For each chunk:
  spawn: git hash-object -w --stdin < chunk_data     → OID
At commit time:
  spawn: git read-tree <parent>
  For each chunk:
    spawn: git update-index --cacheinfo 100644 <oid> <path>
  spawn: git write-tree
  spawn: git commit-tree <tree> -p <parent>
  spawn: git update-ref refs/dolt/data <commit>
```
Total processes: ~2N + 4 (where N = number of chunks)

**With fast-import:**
```
spawn: git fast-import  (SINGLE PROCESS)
  stdin stream:
    blob\nmark :1\ndata <len>\n<chunk1_data>\n
    blob\nmark :2\ndata <len>\n<chunk2_data>\n
    ...
    commit refs/dolt/data\n
    committer ...\n
    data <msg_len>\n<msg>\n
    from <parent_ref>\n
    M 100644 :1 <path1>\n
    M 100644 :2 <path2>\n
    ...
  close stdin, wait for exit
```
Total processes: **1**

**Expected speedup:**
- Eliminates ~2N process spawns
- fast-import is optimized for bulk writes (creates pack files directly, no loose objects)
- On Windows where process spawn is expensive: potentially 10-100x faster for large pushes
- On Linux: 2-10x faster (process spawn cheaper but still significant at scale)

### How fast-import could help FETCH (indirectly)

Fetch already uses `git fetch` for the network transfer, which is efficient. However, the cache population phase (`mergeCacheFromHead`) and chunk reading could benefit from:

1. **`git cat-file --batch`** for reads (not fast-import, but related optimization)
   - Single long-lived process for all blob reads
   - Current: N process spawns for N blobs
   - Improved: 1 process, N reads via stdin/stdout

### What fast-import CANNOT help with
- Network transfer (`git fetch`, `git push`) — these are already single-process
- The push-with-lease semantics — fast-import creates local objects but doesn't push
- The `git push --force-with-lease` call still needed after fast-import

### Implementation Approach

Add a `FastImportWriter` to the GitAPI interface:

```go
// In api.go
type FastImportWriter interface {
    // AddBlob writes a blob and returns a mark ID for later reference
    AddBlob(data io.Reader, size int64) (mark string, err error)
    // AddFileModify records a file modification in the pending commit
    AddFileModify(mode string, mark string, path string)
    // AddFileDelete records a file deletion in the pending commit
    AddFileDelete(path string)
    // Commit creates a commit with all pending modifications
    Commit(ref string, parent *OID, message string, author *Identity) error
    // Close finalizes the fast-import stream and waits for git to finish
    Close() error
}

// In api.go - add to GitAPI interface
StartFastImport(ctx context.Context) (FastImportWriter, error)
```

Modify `buildCommitForKeyWrite()` to use fast-import when available, falling back to the current index-based approach.

### Risks & Considerations

1. **Ref update semantics**: fast-import updates refs directly; need to coordinate with `--force-with-lease` push that follows
2. **Error handling**: fast-import reports errors asynchronously (at stream end); need to handle partial failures
3. **Concurrent access**: fast-import holds a lock on the repo; must not overlap with other git operations
4. **Testing**: Need to verify pack file compatibility across git versions
5. **Incremental writes**: Current deferred-write pattern batches well; fast-import batches even better but requires restructuring the write path

### Also Worth Exploring: `git cat-file --batch`

For reads, a persistent `git cat-file --batch` process would eliminate per-read process spawns:

```go
type BatchCatFile interface {
    // Type returns the object type without reading contents
    Type(oid OID) (string, error)
    // Size returns blob size
    Size(oid OID) (int64, error)
    // Read returns blob contents
    Read(oid OID) (io.ReadCloser, int64, error)
    // Close terminates the batch process
    Close() error
}
```

This is likely a bigger win than fast-import for fetch-heavy workloads.

---

## Recommended Priorities

1. **`git cat-file --batch` for reads** — Biggest bang for buck. Eliminates per-blob process spawns on read path (fetch). Low risk, well-understood git feature.

2. **`git update-index --stdin` / `--index-info` for batch index updates** — Quick win for writes. Replace N `update-index --cacheinfo` calls with a single `update-index --index-info` reading from stdin. Already partially used in `RemoveIndexPaths()`.

3. **`git fast-import` for bulk writes** — Largest potential speedup for push (eliminates hash-object + index + write-tree + commit-tree). Higher implementation complexity but eliminates the most process spawns.

4. **`git hash-object --stdin-paths` or batch mode** — Medium win for the hash-object loop in `planPutWrites()`.

---

## Key Files

| File | Purpose |
|------|---------|
| `go/store/blobstore/internal/git/api.go` | GitAPI interface definition |
| `go/store/blobstore/internal/git/impl.go` | GitAPI implementation (all git exec calls) |
| `go/store/blobstore/internal/git/runner.go` | Process spawning |
| `go/store/blobstore/git_blobstore.go` | Blobstore using git for storage |
| `go/store/nbs/store.go` | NBS chunk store (creates GitStore) |
| `go/libraries/doltcore/dbfactory/git_remote.go` | Git remote factory |
| `go/libraries/doltcore/env/actions/remotes.go` | Fetch/Push orchestration |
| `go/store/datas/pull/puller.go` | Chunk transfer engine |

---

## Verification

To validate any changes:
1. Unit tests: `go test -tags gms_pure_go ./go/store/blobstore/...`
2. Integration: `dolt clone git+file:///tmp/test.git`, `dolt push`, `dolt fetch` against local bare repo
3. Benchmark: Compare process count and wall-clock time before/after with a repo containing 1000+ chunks
