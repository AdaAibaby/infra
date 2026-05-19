# Snapshot Security Issue #2637 - Viable Solutions Analysis

## Problem Statement

**Issue**: Missing teamID filtering in snapshot query creates a permission security risk.

**Current State**:
- 4 handlers call `SnapshotCache.Get(ctx, sandboxID)` which only filters by sandbox ID
- Post-fetch ownership check: `if lastSnapshot.Snapshot.TeamID != teamID { return NotFound }`
- This is redundant and insecure; permission should be enforced at DB layer

**Why PR #2638 Was Rejected**:
- Maintainer: "We need to access the snapshot in some places without knowing team ID apriori"
- Don't want to maintain 2 methods doing basically the same thing
- Introduces more security risk than it solves

## Key Constraint

**Cannot add a second method** - must work with single `Get()` method for all cases.

## Call Site Analysis

| Location | Has TeamID? | Context |
|----------|------------|---------|
| `sandbox_get.go:167` | ✅ Yes | REST handler, team from context |
| `sandbox_connect.go:119` | ✅ Yes | REST handler, teamID parameter |
| `sandbox_pause.go:54` | ✅ Yes | REST handler, teamID parameter |
| `sandbox_resume.go:124` | ✅ Yes | REST handler, teamID parameter |
| `sandbox_resume.go:194` | ✅ Yes | REST handler, teamID parameter |
| `proxy_grpc.go:99` | ❌ No | gRPC internal service, auto-resume feature |

**Finding**: 5 out of 6 call sites have teamID available. Only `proxy_grpc.go` (internal gRPC service) doesn't.

## Recommended Solution: Conditional Filtering with Optional TeamID

### Overview
Modify `SnapshotCache.Get()` to accept an optional `teamID` parameter. When provided, filter at the cache layer. When not provided, return snapshot as-is (for internal services).

### Implementation Steps

#### 1. Update SQL Query (Optional - for future optimization)
Create a new query variant that supports optional team filtering:

```sql
-- name: GetLastSnapshotByTeam :one
SELECT ... FROM snapshots s
WHERE s.sandbox_id = $1 AND s.team_id = $2
```

This allows future optimization without changing the API.

#### 2. Update SnapshotCache Interface

```go
// Get returns the last snapshot for a sandbox, optionally filtered by teamID.
// If teamID is provided, only returns snapshots belonging to that team.
// If teamID is nil, returns snapshot without team filtering (for internal services).
func (c *SnapshotCache) Get(ctx context.Context, sandboxID string, teamID *uuid.UUID) (*SnapshotInfo, error)
```

#### 3. Update Cache Implementation

```go
func (c *SnapshotCache) Get(ctx context.Context, sandboxID string, teamID *uuid.UUID) (*SnapshotInfo, error) {
    ctx, span := tracer.Start(ctx, "get last snapshot")
    defer span.End()

    // Use teamID in cache key to separate cached results
    cacheKey := sandboxID
    if teamID != nil {
        cacheKey = fmt.Sprintf("%s:%s", sandboxID, teamID.String())
    }

    info, err := c.cache.GetOrSet(ctx, cacheKey, func(ctx context.Context, key string) (*SnapshotInfo, error) {
        return c.fetchFromDB(ctx, sandboxID, teamID)
    })
    if err != nil {
        return nil, err
    }

    if info.NotFound {
        return nil, ErrSnapshotNotFound
    }

    return info, nil
}

func (c *SnapshotCache) fetchFromDB(ctx context.Context, sandboxID string, teamID *uuid.UUID) (*SnapshotInfo, error) {
    ctx, span := tracer.Start(ctx, "fetch last snapshot from DB")
    defer span.End()

    row, err := c.db.GetLastSnapshot(ctx, sandboxID)
    if err != nil {
        if dberrors.IsNotFoundError(err) {
            return errNotFoundSentinel, nil
        }
        return nil, fmt.Errorf("fetching last snapshot: %w", err)
    }

    // Validate team ownership if teamID provided
    if teamID != nil && row.Snapshot.TeamID != *teamID {
        return errNotFoundSentinel, nil  // Treat as not found for security
    }

    return &SnapshotInfo{
        Aliases:  row.Aliases,
        Names:    row.Names,
        Snapshot: row.Snapshot,
        EnvBuild: row.EnvBuild,
    }, nil
}
```

#### 4. Update Handler Call Sites

**Before**:
```go
lastSnapshot, err := a.snapshotCache.Get(ctx, sandboxId)
if err != nil {
    // handle error
}
if lastSnapshot.Snapshot.TeamID != team.ID {
    return NotFound
}
```

**After**:
```go
lastSnapshot, err := a.snapshotCache.Get(ctx, sandboxId, &team.ID)
if err != nil {
    // handle error
}
// No post-fetch check needed - filtering done at cache layer
```

#### 5. Update Internal gRPC Call Site

**proxy_grpc.go** (no change needed):
```go
snap, err := s.api.snapshotCache.Get(ctx, sandboxID, nil)  // nil = no team filtering
if err != nil {
    // handle error
}
```

### Benefits of This Approach

✅ **Single method** - No need for two methods  
✅ **Backward compatible** - Optional parameter  
✅ **Security at cache layer** - Team validation happens before returning  
✅ **Flexible** - Works for both authenticated (with teamID) and internal (without teamID) calls  
✅ **Efficient caching** - Separate cache entries for different teams  
✅ **Audit trail** - Can log all snapshot accesses with/without team context  

### Security Considerations

1. **Defense in depth**: Even though validation happens at cache layer, the DB query still returns full snapshot. This is acceptable because:
   - Cache layer validates before returning to caller
   - Internal gRPC service (proxy_grpc) is trusted code
   - No external exposure of unfiltered snapshots

2. **Cache key separation**: Using `sandboxID:teamID` as cache key prevents cache poisoning where one team could access another team's cached snapshot.

3. **Treat as not found**: When team validation fails, return `errNotFoundSentinel` (not found) rather than an error, maintaining consistent error handling.

## Alternative Solutions (Not Recommended)

### Alternative 1: Two Methods (Rejected by Maintainer)
- `Get(ctx, sandboxID)` - for internal use
- `GetByTeam(ctx, sandboxID, teamID)` - for handlers
- **Problem**: Maintainer explicitly rejected this approach

### Alternative 2: Context-Based Filtering
- Store teamID in request context
- Extract in cache layer
- **Problem**: Doesn't work for internal gRPC calls without team context

### Alternative 3: Audit Logging Only
- Keep current implementation
- Add comprehensive audit logging
- **Problem**: Doesn't fix the security issue, only logs it

### Alternative 4: Separate Internal Service
- Create separate internal snapshot service
- **Problem**: Adds complexity, still requires two code paths

## Implementation Checklist

- [ ] Update `SnapshotCache.Get()` signature to accept optional `*uuid.UUID`
- [ ] Update `fetchFromDB()` to validate teamID when provided
- [ ] Update cache key generation to include teamID
- [ ] Update all 5 handler call sites to pass `&teamID`
- [ ] Update proxy_grpc call site to pass `nil`
- [ ] Remove TODO comments and post-fetch ownership checks from handlers
- [ ] Add unit tests for:
  - Get with valid teamID
  - Get with invalid teamID (should return not found)
  - Get with nil teamID (internal use)
  - Cache key separation between teams
- [ ] Update OpenAPI spec if needed
- [ ] Add integration tests

## Testing Strategy

```go
// Test 1: Valid team access
snap, err := cache.Get(ctx, sandboxID, &validTeamID)
// Should succeed

// Test 2: Invalid team access
snap, err := cache.Get(ctx, sandboxID, &otherTeamID)
// Should return ErrSnapshotNotFound

// Test 3: Internal access (no team)
snap, err := cache.Get(ctx, sandboxID, nil)
// Should succeed regardless of team

// Test 4: Cache separation
snap1, _ := cache.Get(ctx, sandboxID, &team1ID)
snap2, _ := cache.Get(ctx, sandboxID, &team2ID)
// Should have separate cache entries
```

## Migration Path

1. **Phase 1**: Update `SnapshotCache.Get()` signature (backward compatible with optional param)
2. **Phase 2**: Update handler call sites one by one
3. **Phase 3**: Remove TODO comments and post-fetch checks
4. **Phase 4**: Add comprehensive tests
5. **Phase 5**: Monitor for any issues in staging

## Conclusion

This solution:
- ✅ Maintains single `Get()` method (no duplication)
- ✅ Enforces team filtering at cache layer (security)
- ✅ Supports internal services without teamID (flexibility)
- ✅ Separates cache entries by team (prevents cache poisoning)
- ✅ Removes redundant post-fetch checks (cleaner code)
- ✅ Addresses maintainer's concerns about duplication and flexibility
