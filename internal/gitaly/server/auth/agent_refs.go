package auth

import (
	"context"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/feder-cr/git-ai/internal/graft/agentrefs"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// The agent reference-visibility filter (agentrefs): an agent neither names nor lists a reference
// under refs/graft/. It is applied here, once, to every request and response of an agent:
//   - a request field that names a revision, a reference or a reference pattern is refused when it
//     names refs/graft/ (agentrefs.RevisionHidden);
//   - a response loses every reference under refs/graft/ it lists: a list element that is such a
//     name, or a message that names one in a reference field.
// Which fields name references is decided by agentRefField: an exact table for the messages of
// the RPCs agents may call by default (a test fails on any string or bytes field that the table
// does not classify), and a conservative rule by field name for the RPCs an operator adds with
// [graft] agent_rpcs.

// refFieldKind is what a string or bytes field of a request or a response holds.
type refFieldKind int

const (
	notRef refFieldKind = iota
	// isRef: a revision, a reference or a reference pattern: checked, or filtered.
	isRef
	// isBranchName: a branch name the server completes under refs/heads/ when it is short; only a
	// full name under refs/graft/ is refused (no revision rule completes it to refs/graft/).
	isBranchName
)

// agentRefFieldsByName classifies a field by its name alone.
var agentRefFieldsByName = map[protoreflect.Name]refFieldKind{
	// Revisions, references and patterns of requests, and references of responses.
	"commit": isRef, "commit_id": isRef, "commit_ids": isRef, "commit_revision": isRef, "commits": isRef,
	"expected_head_commit_id": isRef, "expected_old_oid": isRef, "head_commit_id": isRef, "base_commit_id": isRef,
	"include_referenced_by": isRef, "left_commit_id": isRef, "right_commit_id": isRef,
	"left_tree_revision": isRef, "right_tree_revision": isRef, "new_revision": isRef, "old_revision": isRef,
	"oid": isRef, "oids": isRef, "parent_commit_revisions": isRef, "patterns": isRef, "pointing_at_oids": isRef,
	"ref_names": isRef, "ref_patterns": isRef, "resume_oid": isRef, "revision": isRef, "revisions": isRef,
	"start_point": isRef, "start_sha": isRef, "tag_ids": isRef, "tag_revisions": isRef, "tip": isRef,
	"tree_oid": isRef, "blob_ids": isRef, "refs": isRef, "ref_name": isRef, "reference": isRef,
	"referenced_by": isRef, "branch_names": isRef, "tag_names": isRef, "target": isRef,
	// Branch and tag names: the server completes them under refs/heads/ and refs/tags/, so they
	// cannot name refs/graft/; and fields that hold no reference.
	"branch": notRef, "branch_name": notRef, "start_branch_name": notRef, "target_branch": notRef,
	"merged_branches": notRef, "tag_name": notRef, "remote_name": notRef, "exclude_patterns": notRef,
	"author": notRef, "body": notRef, "commit_author_email": notRef, "commit_author_name": notRef,
	"commit_message": notRef, "commit_message_patterns": notRef, "content": notRef, "description": notRef,
	"email": notRef, "file_path": notRef, "git_alternate_object_directories": notRef,
	"git_object_directory": notRef, "gl_id": notRef, "gl_project_path": notRef, "gl_repository": notRef,
	"gl_username": notRef, "id": notRef, "merge_message": notRef, "message_regex": notRef, "mode": notRef,
	"page_token": notRef, "path": notRef, "paths": notRef, "previous_path": notRef, "query": notRef,
	"relative_path": notRef, "sort_field": notRef, "storage_name": notRef, "timezone": notRef, "title": notRef,
	"actor": notRef, "commit_oid": notRef, "data": notRef, "encoding": notRef, "file_oid": notRef,
	"filesystem_id": notRef, "flat_path": notRef, "following_lines": notRef, "fs_type": notRef,
	"git_version": notRef, "index_error": notRef, "key": notRef, "landed_commit_id": notRef, "line": notRef,
	"message": notRef, "new_blob_id": notRef, "new_oid": notRef, "next_cursor": notRef, "old_blob_id": notRef,
	"old_oid": notRef, "old_path": notRef, "old_tip": notRef, "operation": notRef, "original_commit_id": notRef,
	"parent_ids": notRef, "patch_id": notRef, "path_bytes": notRef, "peeled_target": notRef,
	"pre_receive_error": notRef, "preceding_lines": notRef, "reason": notRef, "resolved_paths": notRef,
	"resolver": notRef, "rule": notRef, "server_version": notRef, "signature": notRef, "signed_text": notRef,
	"subject": notRef, "tag_id": notRef, "tree_id": notRef, "value": notRef,
}

// agentRefNameFields classifies the field "name", whose meaning depends on its message.
var agentRefNameFields = map[protoreflect.FullName]refFieldKind{
	"gitaly.FindBranchRequest":              isBranchName,
	"gitaly.Branch":                         isRef,
	"gitaly.FindAllBranchesResponse.Branch": isRef,
	"gitaly.FindDefaultBranchNameResponse":  isRef,
	"gitaly.ListRefsResponse.Reference":     isRef,
	"gitaly.Tag":                            isRef,
	"gitaly.User":                           notRef,
	"gitaly.CommitAuthor":                   notRef,
}

// agentRefField returns what a string or bytes field holds, and whether the exact table knows it.
func agentRefField(md protoreflect.MessageDescriptor, fd protoreflect.FieldDescriptor) (refFieldKind, bool) {
	if fd.Name() == "name" {
		kind, ok := agentRefNameFields[md.FullName()]
		if ok {
			return kind, true
		}
	} else if kind, ok := agentRefFieldsByName[fd.Name()]; ok {
		return kind, true
	}
	// A field of an RPC an operator added: a name that may hold a revision, a reference or a
	// pattern is checked.
	n := string(fd.Name())
	for _, part := range []string{"revision", "ref", "pattern", "commit", "oid", "sha", "branch", "tag", "name", "target"} {
		if strings.Contains(n, part) {
			return isRef, false
		}
	}
	return notRef, false
}

// isAgent reports whether the caller of ctx authenticated with a per-agent token.
func isAgent(ctx context.Context) (string, bool) {
	id, ok := identity.FromContext(ctx)
	return id.Name, ok && id.Source == identity.SourceAgent
}

// checkAgentRefs refuses a request of an agent that names a reference under refs/graft/.
func checkAgentRefs(ctx context.Context, req any) error {
	name, ok := isAgent(ctx)
	if !ok {
		return nil
	}
	msg, ok := req.(proto.Message)
	if !ok {
		return nil
	}
	if field, value, hidden := findHiddenRef(msg.ProtoReflect()); hidden {
		return structerr.NewPermissionDenied("agent %q may not name %q (%s): references under %s are internal to the server",
			name, value, field, agentrefs.HiddenPrefix)
	}
	return nil
}

// findHiddenRef returns the first reference field of m, at any depth, whose value names refs/graft/.
func findHiddenRef(m protoreflect.Message) (protoreflect.FullName, string, bool) {
	var field protoreflect.FullName
	var value string
	var hidden bool
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch fd.Kind() {
		case protoreflect.StringKind, protoreflect.BytesKind:
			kind, _ := agentRefField(m.Descriptor(), fd)
			if kind == notRef {
				return true
			}
			for _, s := range scalarStrings(fd, v) {
				if (kind == isRef && agentrefs.RevisionHidden(s)) || (kind == isBranchName && agentrefs.Hidden(s)) {
					field, value, hidden = fd.FullName(), s, true
					return false
				}
			}
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if fd.IsMap() {
				return true
			}
			for _, sub := range subMessages(fd, v) {
				if f, val, h := findHiddenRef(sub); h {
					field, value, hidden = f, val, true
					return false
				}
			}
		}
		return true
	})
	return field, value, hidden
}

// filterAgentRefs removes from a response to an agent every reference under refs/graft/ it lists.
func filterAgentRefs(ctx context.Context, resp any) {
	if _, ok := isAgent(ctx); !ok {
		return
	}
	if msg, ok := resp.(proto.Message); ok && msg != nil {
		filterMessage(msg.ProtoReflect(), true)
	}
}

// filterMessage filters m in place, and reports whether m itself names a hidden reference in a
// singular reference field, so that the list holding it drops it. At the top level such a field
// is cleared instead. The changes are collected while ranging over the fields and applied after.
func filterMessage(m protoreflect.Message, top bool) bool {
	drop := false
	type change struct {
		fd    protoreflect.FieldDescriptor
		value protoreflect.Value
		clear bool
	}
	var changes []change
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch fd.Kind() {
		case protoreflect.StringKind, protoreflect.BytesKind:
			if kind, _ := agentRefField(m.Descriptor(), fd); kind == notRef {
				return true
			}
			if fd.IsList() {
				list := v.List()
				kept := m.NewField(fd).List()
				for i := 0; i < list.Len(); i++ {
					if !agentrefs.Hidden(valueString(fd, list.Get(i))) {
						kept.Append(list.Get(i))
					}
				}
				if kept.Len() != list.Len() {
					changes = append(changes, change{fd: fd, value: protoreflect.ValueOfList(kept), clear: kept.Len() == 0})
				}
			} else if agentrefs.Hidden(valueString(fd, v)) {
				if top {
					changes = append(changes, change{fd: fd, clear: true})
				} else {
					drop = true
				}
			}
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if fd.IsMap() {
				return true
			}
			if fd.IsList() {
				list := v.List()
				kept := m.NewField(fd).List()
				for i := 0; i < list.Len(); i++ {
					if !filterMessage(list.Get(i).Message(), false) {
						kept.Append(list.Get(i))
					}
				}
				if kept.Len() != list.Len() {
					changes = append(changes, change{fd: fd, value: protoreflect.ValueOfList(kept), clear: kept.Len() == 0})
				}
			} else if filterMessage(v.Message(), false) {
				changes = append(changes, change{fd: fd, clear: true})
			}
		}
		return true
	})
	for _, c := range changes {
		if c.clear {
			m.Clear(c.fd)
		} else {
			m.Set(c.fd, c.value)
		}
	}
	return drop
}

func scalarStrings(fd protoreflect.FieldDescriptor, v protoreflect.Value) []string {
	if !fd.IsList() {
		return []string{valueString(fd, v)}
	}
	list := v.List()
	out := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		out = append(out, valueString(fd, list.Get(i)))
	}
	return out
}

func valueString(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	if fd.Kind() == protoreflect.BytesKind {
		return string(v.Bytes())
	}
	return v.String()
}

func subMessages(fd protoreflect.FieldDescriptor, v protoreflect.Value) []protoreflect.Message {
	if !fd.IsList() {
		return []protoreflect.Message{v.Message()}
	}
	list := v.List()
	out := make([]protoreflect.Message, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		out = append(out, list.Get(i).Message())
	}
	return out
}
