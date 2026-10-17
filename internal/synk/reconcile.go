package synk

import (
	"sort"
	"strings"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

// OpKind is the kind of action a reconciliation produced.
type OpKind string

const (
	OpMkdirLocal  OpKind = "mkdir-local"
	OpMkdirRemote OpKind = "mkdir-remote"
	OpDownload    OpKind = "download"
	OpUpload      OpKind = "upload"
	OpTrashLocal  OpKind = "trash-local"
	OpTrashRemote OpKind = "trash-remote"
	OpRecord      OpKind = "record" // already in sync, state needs the ids
	OpForget      OpKind = "forget" // gone on both sides
	OpSkip        OpKind = "skip"   // cannot be represented on one of the sides
)

// Winner of a conflict.
type Winner string

const (
	WinnerNone   Winner = ""
	WinnerLocal  Winner = "local"
	WinnerRemote Winner = "remote"
)

// Op is a single action to apply.
type Op struct {
	Kind   OpKind
	Path   string // slash-separated, relative to the Drive root
	Local  LocalFile
	Remote icloud.Item

	// Conflict is set when both sides changed; Winner says which one is kept.
	Conflict bool
	Winner   Winner
	Note     string
}

// Policy is the subset of configuration the reconciler needs.
type Policy struct {
	DeleteLocal  bool
	DeleteRemote bool
	// ProtectDeletions skips previously-synced paths missing on one side instead of
	// automatically deleting or recreating the missing counterpart.
	ProtectDeletions bool
	// ProtectedPaths contains local paths intentionally omitted from the scan,
	// such as files exceeding max_file_size. They must not trigger reconciliation.
	ProtectedPaths map[string]bool
	// Conflict is "both", "newer" or "remote".
	Conflict string
}

// Reconcile compares the local tree, the remote tree and the state recorded
// after the previous pass, and returns the actions needed to converge.
//
// The state is what makes this a synchronisation rather than a mirror: a path
// present on one side only is a creation if the state has never seen it, and a
// deletion if it has.
func Reconcile(local map[string]LocalFile, remote map[string]icloud.Item, state map[string]Entry, p Policy) []Op {
	paths := make(map[string]struct{}, len(local)+len(remote)+len(state))
	for k := range local {
		paths[k] = struct{}{}
	}
	for k := range remote {
		paths[k] = struct{}{}
	}
	for k := range state {
		paths[k] = struct{}{}
	}

	var ops []Op
	for path := range paths {
		l, hasL := local[path]
		r, hasR := remote[path]
		st, hasS := state[path]

		if p.ProtectedPaths[path] {
			ops = append(ops, Op{
				Kind: OpSkip, Path: path,
				Note: "local file exceeds max_file_size; reconciliation protected",
			})
			continue
		}

		// Internal replacement files remain visible in the inventories, but
		// must never trigger normal synchronization or state-forgetting actions.
		if isReplacementSiblingName(path) {
			ops = append(ops, Op{
				Kind: OpSkip, Path: path,
				Local: l, Remote: r,
				Note: "internal replacement file retained; automatic reconciliation skipped",
			})
			continue
		}

		switch {
		// ---------------------------------------------------------- directories
		case hasR && r.IsDir():
			if !hasL {
				if p.ProtectDeletions && hasS && st.IsDir {
					ops = append(ops, Op{Kind: OpSkip, Path: path, Remote: r,
						Note: "previously synced local directory is missing; deletion protected"})
				} else {
					ops = append(ops, Op{Kind: OpMkdirLocal, Path: path, Remote: r})
				}
				continue
			}
			if !l.IsDir {
				ops = append(ops, Op{Kind: OpSkip, Path: path, Local: l, Remote: r,
					Note: "directory in iCloud but a file locally"})
				continue
			}
			ops = append(ops, Op{Kind: OpRecord, Path: path, Local: l, Remote: r})

		case hasL && l.IsDir:
			if hasS && st.IsDir && st.DrivewsID != "" {
				// The directory was synced before and is gone from iCloud.
				if p.ProtectDeletions {
					ops = append(ops, Op{Kind: OpSkip, Path: path, Local: l,
						Note: "previously synced iCloud directory is missing; deletion protected"})
				} else if p.DeleteLocal {
					ops = append(ops, Op{Kind: OpTrashLocal, Path: path, Local: l,
						Note: "directory removed in iCloud"})
				} else {
					ops = append(ops, Op{Kind: OpMkdirRemote, Path: path, Local: l})
				}
				continue
			}
			ops = append(ops, Op{Kind: OpMkdirRemote, Path: path, Local: l})

		case hasS && st.IsDir && !hasL && !hasR:
			ops = append(ops, Op{Kind: OpForget, Path: path})

		// --------------------------------------------------------------- files
		case hasL && hasR:
			if r.Type == icloud.TypePackage || r.ItemCount > 0 {
				ops = append(ops, Op{Kind: OpSkip, Path: path, Local: l, Remote: r,
					Note: "iCloud package (bundle) — not synced"})
				continue
			}
			lc := localChanged(l, st, hasS)
			rc := remoteChanged(r, st, hasS)
			switch {
			case lc && rc:
				ops = append(ops, conflictOp(path, l, r, p))
			case lc:
				ops = append(ops, Op{Kind: OpUpload, Path: path, Local: l, Remote: r})
			case rc:
				ops = append(ops, Op{Kind: OpDownload, Path: path, Local: l, Remote: r})
			default:
				ops = append(ops, Op{Kind: OpRecord, Path: path, Local: l, Remote: r})
			}

		case hasL && !hasR:
			if hasS && st.DrivewsID != "" {
				if p.ProtectDeletions {
					ops = append(ops, Op{Kind: OpSkip, Path: path, Local: l,
						Note: "previously synced iCloud file is missing; deletion protected"})
				} else if p.DeleteLocal {
					// It was in iCloud and is gone: the deletion happened there.
					ops = append(ops, Op{Kind: OpTrashLocal, Path: path, Local: l,
						Note: "removed in iCloud"})
				} else {
					ops = append(ops, Op{Kind: OpUpload, Path: path, Local: l})
				}
				continue
			}
			ops = append(ops, Op{Kind: OpUpload, Path: path, Local: l})

		case !hasL && hasR:
			if r.Type == icloud.TypePackage || r.ItemCount > 0 {
				ops = append(ops, Op{Kind: OpSkip, Path: path, Remote: r,
					Note: "iCloud package (bundle) — not synced"})
				continue
			}
			if hasS && !st.LocalMTime.IsZero() {
				if p.ProtectDeletions {
					ops = append(ops, Op{Kind: OpSkip, Path: path, Remote: r,
						Note: "previously synced local file is missing; deletion protected"})
				} else if p.DeleteRemote {
					ops = append(ops, Op{Kind: OpTrashRemote, Path: path, Remote: r,
						Note: "removed locally"})
				} else {
					ops = append(ops, Op{Kind: OpDownload, Path: path, Remote: r})
				}
				continue
			}
			ops = append(ops, Op{Kind: OpDownload, Path: path, Remote: r})

		case hasS:
			ops = append(ops, Op{Kind: OpForget, Path: path})
		}
	}

	sortOps(ops)
	return ops
}

// localChanged reports whether the local file differs from what we recorded.
func localChanged(l LocalFile, st Entry, hasS bool) bool {
	if !hasS {
		return true
	}
	if l.Size != st.Size {
		return true
	}
	// Filesystem timestamps have finer resolution than we store; compare at
	// second granularity to avoid spurious uploads.
	return !l.MTime.Truncate(time.Second).Equal(st.LocalMTime.Truncate(time.Second))
}

// remoteChanged reports whether the iCloud item differs from what we recorded.
// ETag is the primary signal; size is a secondary signal if the ETag is unchanged.
func remoteChanged(r icloud.Item, st Entry, hasS bool) bool {
	if !hasS {
		return true
	}
	if st.Etag == "" {
		return true
	}
	if r.Etag != st.Etag {
		return true
	}
	return r.Size != st.Size
}

// conflictOp decides who wins when both sides changed.
func conflictOp(path string, l LocalFile, r icloud.Item, p Policy) Op {
	op := Op{Path: path, Local: l, Remote: r, Conflict: true}
	switch p.Conflict {
	case "remote":
		op.Kind = OpDownload
		op.Winner = WinnerRemote
		op.Note = "conflict: iCloud version kept"
		return op
	default: // "both" and "newer" agree on the winner; they differ in apply.
		if l.MTime.After(r.ModTime()) {
			op.Kind = OpUpload
			op.Winner = WinnerLocal
			op.Note = "conflict: local version is newer"
		} else {
			op.Kind = OpDownload
			op.Winner = WinnerRemote
			op.Note = "conflict: iCloud version is newer"
		}
		return op
	}
}

// sortOps orders operations so they can be applied safely: parents are created
// before children, transfers follow, and deletions go deepest-first.
func sortOps(ops []Op) {
	rank := func(k OpKind) int {
		switch k {
		case OpMkdirLocal, OpMkdirRemote:
			return 0
		case OpDownload, OpUpload:
			return 1
		case OpRecord:
			return 2
		case OpTrashLocal, OpTrashRemote:
			return 3
		case OpSkip:
			return 4
		default: // OpForget
			return 5
		}
	}
	sort.SliceStable(ops, func(i, j int) bool {
		ri, rj := rank(ops[i].Kind), rank(ops[j].Kind)
		if ri != rj {
			return ri < rj
		}
		di, dj := depth(ops[i].Path), depth(ops[j].Path)
		if di != dj {
			if ri == 3 { // deletions: children first
				return di > dj
			}
			return di < dj
		}
		return ops[i].Path < ops[j].Path
	})
}

func depth(p string) int { return strings.Count(p, "/") }
