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
// Which fields name references is decided by agentRefField: an exact table, by message and field,
// for the messages of the RPCs agents may call by default (a test fails on any string, bytes, bool
// or enum field of theirs that the table does not classify), and a conservative rule by field name
// for the RPCs an operator adds with [graft] agent_rpcs. A flag that makes git walk every reference
// (walksAllRefs) is not checked here: gitcmd leaves the hidden references out of every walk of an
// agent's command (gitcmd/agent_walk.go), whatever asked for it.

// refFieldKind is what a field of a request or a response holds, or does.
type refFieldKind int

const (
	notRef refFieldKind = iota
	// isRef: a revision, a reference or a reference pattern: checked, or filtered.
	isRef
	// isBranchName: a branch name the server completes under refs/heads/ when it is short; only a
	// full name under refs/graft/ is refused (no revision rule completes it to refs/graft/).
	isBranchName
	// walksAllRefs: a flag that makes git walk every reference (git's --all); the walk of an agent
	// leaves out the hidden references (gitcmd).
	walksAllRefs
)

// agentRefFields classifies, by message and field, the string, bytes, bool and enum fields of the
// requests and responses of the RPCs agents may call by default (builtinAgentRPCs), at any depth.
// Numeric fields name no reference and select no walk, and are not listed.
var agentRefFields = map[protoreflect.FullName]refFieldKind{
	"gitaly.AddReviewCommentRequest.body": notRef,
	"gitaly.AddReviewCommentRequest.id":   notRef,
	"gitaly.AddReviewCommentRequest.path": notRef,

	"gitaly.BlameRequest.path":     notRef,
	"gitaly.BlameRequest.revision": isRef,

	"gitaly.BlameResponse.Line.commit_id": isRef,
	"gitaly.BlameResponse.Line.content":   notRef,

	"gitaly.BlameResponse.commit_id": isRef,

	"gitaly.Branch.name": isRef,

	"gitaly.ChangedPaths.commit_id":   isRef,
	"gitaly.ChangedPaths.new_blob_id": notRef,
	"gitaly.ChangedPaths.old_blob_id": notRef,
	"gitaly.ChangedPaths.old_path":    notRef,
	"gitaly.ChangedPaths.path":        notRef,
	"gitaly.ChangedPaths.status":      notRef,

	"gitaly.Changeset.actor":     notRef,
	"gitaly.Changeset.new_oid":   notRef,
	"gitaly.Changeset.old_oid":   notRef,
	"gitaly.Changeset.operation": notRef,
	"gitaly.Changeset.reference": isRef,
	"gitaly.Changeset.rewritten": notRef,

	"gitaly.ChangesetMember.actor":              notRef,
	"gitaly.ChangesetMember.commit_id":          isRef,
	"gitaly.ChangesetMember.original_commit_id": notRef,
	"gitaly.ChangesetMember.resolved_paths":     notRef,
	"gitaly.ChangesetMember.rewritten":          notRef,

	"gitaly.CloseReviewRequest.id":     notRef,
	"gitaly.CloseReviewRequest.landed": notRef,

	"gitaly.CommitAuthor.email":    notRef,
	"gitaly.CommitAuthor.name":     notRef,
	"gitaly.CommitAuthor.timezone": notRef,

	"gitaly.CommitTrailer.key":   notRef,
	"gitaly.CommitTrailer.value": notRef,

	"gitaly.CreateReviewRequest.base_commit_id": isRef,
	"gitaly.CreateReviewRequest.description":    notRef,
	"gitaly.CreateReviewRequest.head_commit_id": isRef,
	"gitaly.CreateReviewRequest.target_branch":  notRef,
	"gitaly.CreateReviewRequest.title":          notRef,

	"gitaly.DiffStats.old_path": notRef,
	"gitaly.DiffStats.path":     notRef,

	"gitaly.DiffStatsRequest.left_commit_id":     isRef,
	"gitaly.DiffStatsRequest.right_commit_id":    isRef,
	"gitaly.DiffStatsRequest.whitespace_changes": notRef,

	"gitaly.FindAllBranchesRequest.merged_branches": notRef,
	"gitaly.FindAllBranchesRequest.merged_only":     notRef,

	"gitaly.FindAllBranchesResponse.Branch.name": isRef,

	"gitaly.FindAllRemoteBranchesRequest.remote_name": notRef,

	"gitaly.FindAllTagsRequest.SortBy.direction": notRef,
	"gitaly.FindAllTagsRequest.SortBy.key":       notRef,

	"gitaly.FindBranchRequest.name": isBranchName,

	"gitaly.FindChangedPathsRequest.Request.CommitRequest.commit_revision":         isRef,
	"gitaly.FindChangedPathsRequest.Request.CommitRequest.parent_commit_revisions": isRef,

	"gitaly.FindChangedPathsRequest.Request.TreeRequest.left_tree_revision":  isRef,
	"gitaly.FindChangedPathsRequest.Request.TreeRequest.right_tree_revision": isRef,

	"gitaly.FindChangedPathsRequest.commits":                isRef,
	"gitaly.FindChangedPathsRequest.diff_filters":           notRef,
	"gitaly.FindChangedPathsRequest.find_renames":           notRef,
	"gitaly.FindChangedPathsRequest.merge_commit_diff_mode": notRef,

	"gitaly.FindCommitRequest.revision": isRef,
	"gitaly.FindCommitRequest.trailers": notRef,

	"gitaly.FindCommitsRequest.all":                   walksAllRefs,
	"gitaly.FindCommitsRequest.author":                notRef,
	"gitaly.FindCommitsRequest.disable_walk":          notRef,
	"gitaly.FindCommitsRequest.first_parent":          notRef,
	"gitaly.FindCommitsRequest.follow":                notRef,
	"gitaly.FindCommitsRequest.include_referenced_by": isRef,
	"gitaly.FindCommitsRequest.include_shortstat":     notRef,
	"gitaly.FindCommitsRequest.message_regex":         notRef,
	"gitaly.FindCommitsRequest.order":                 notRef,
	"gitaly.FindCommitsRequest.paths":                 notRef,
	"gitaly.FindCommitsRequest.revision":              isRef,
	"gitaly.FindCommitsRequest.skip_merges":           notRef,
	"gitaly.FindCommitsRequest.trailers":              notRef,

	"gitaly.FindDefaultBranchNameRequest.head_only": notRef,

	"gitaly.FindDefaultBranchNameResponse.name": isRef,

	"gitaly.FindLocalBranchesRequest.exclude_patterns": notRef,
	"gitaly.FindLocalBranchesRequest.sort_by":          notRef,

	"gitaly.FindRefsByOIDRequest.oid":          isRef,
	"gitaly.FindRefsByOIDRequest.ref_patterns": isRef,
	"gitaly.FindRefsByOIDRequest.sort_field":   notRef,

	"gitaly.FindRefsByOIDResponse.refs": isRef,

	"gitaly.FindTagRequest.tag_name": notRef,

	"gitaly.GetBlobRequest.oid": isRef,

	"gitaly.GetBlobResponse.data": notRef,
	"gitaly.GetBlobResponse.oid":  isRef,

	"gitaly.GetBlobsRequest.RevisionPath.path":     notRef,
	"gitaly.GetBlobsRequest.RevisionPath.revision": isRef,

	"gitaly.GetBlobsResponse.data":         notRef,
	"gitaly.GetBlobsResponse.is_submodule": notRef,
	"gitaly.GetBlobsResponse.oid":          isRef,
	"gitaly.GetBlobsResponse.path":         notRef,
	"gitaly.GetBlobsResponse.revision":     isRef,
	"gitaly.GetBlobsResponse.type":         notRef,

	"gitaly.GetCommitMessagesRequest.commit_ids": isRef,

	"gitaly.GetCommitMessagesResponse.commit_id": isRef,
	"gitaly.GetCommitMessagesResponse.message":   notRef,

	"gitaly.GetCommitSignaturesRequest.commit_ids": isRef,

	"gitaly.GetCommitSignaturesResponse.commit_id":   isRef,
	"gitaly.GetCommitSignaturesResponse.signature":   notRef,
	"gitaly.GetCommitSignaturesResponse.signed_text": notRef,
	"gitaly.GetCommitSignaturesResponse.signer":      notRef,

	"gitaly.GetLFSPointersRequest.blob_ids": isRef,

	"gitaly.GetObjectsRequest.oids":       isRef,
	"gitaly.GetObjectsRequest.resume_oid": isRef,

	"gitaly.GetObjectsResponse.data":    notRef,
	"gitaly.GetObjectsResponse.missing": notRef,
	"gitaly.GetObjectsResponse.oid":     isRef,
	"gitaly.GetObjectsResponse.type":    notRef,

	"gitaly.GetPatchIDRequest.context_free":    notRef,
	"gitaly.GetPatchIDRequest.new_revision":    isRef,
	"gitaly.GetPatchIDRequest.old_revision":    isRef,
	"gitaly.GetPatchIDRequest.whole_diff_hash": notRef,

	"gitaly.GetPatchIDResponse.patch_id": notRef,

	"gitaly.GetReviewRequest.id": notRef,

	"gitaly.GetTagMessagesRequest.tag_ids": isRef,

	"gitaly.GetTagMessagesResponse.message": notRef,
	"gitaly.GetTagMessagesResponse.tag_id":  notRef,

	"gitaly.GetTagSignaturesRequest.tag_revisions": isRef,

	"gitaly.GetTagSignaturesResponse.TagSignature.content":   notRef,
	"gitaly.GetTagSignaturesResponse.TagSignature.signature": notRef,
	"gitaly.GetTagSignaturesResponse.TagSignature.tag_id":    notRef,

	"gitaly.GetTreeEntriesRequest.path":             notRef,
	"gitaly.GetTreeEntriesRequest.recursive":        notRef,
	"gitaly.GetTreeEntriesRequest.revision":         isRef,
	"gitaly.GetTreeEntriesRequest.skip_flat_paths":  notRef,
	"gitaly.GetTreeEntriesRequest.sort":             notRef,
	"gitaly.GetTreeEntriesRequest.with_last_commit": notRef,

	"gitaly.GetTreeRequest.include_sizes": notRef,
	"gitaly.GetTreeRequest.path":          notRef,
	"gitaly.GetTreeRequest.recursive":     notRef,
	"gitaly.GetTreeRequest.revision":      isRef,
	"gitaly.GetTreeRequest.tree_oid":      isRef,

	"gitaly.GetTreeResponse.Entry.oid":  isRef,
	"gitaly.GetTreeResponse.Entry.path": notRef,
	"gitaly.GetTreeResponse.Entry.type": notRef,

	"gitaly.GetTreeResponse.tree_oid": isRef,

	"gitaly.GitCommit.body":           notRef,
	"gitaly.GitCommit.encoding":       notRef,
	"gitaly.GitCommit.id":             notRef,
	"gitaly.GitCommit.parent_ids":     notRef,
	"gitaly.GitCommit.referenced_by":  isRef,
	"gitaly.GitCommit.signature_type": notRef,
	"gitaly.GitCommit.subject":        notRef,
	"gitaly.GitCommit.tree_id":        notRef,

	"gitaly.GlobalOptions.literal_pathspecs": notRef,

	"gitaly.LFSPointer.data":     notRef,
	"gitaly.LFSPointer.file_oid": notRef,
	"gitaly.LFSPointer.oid":      isRef,

	"gitaly.LandResolution.commit": isRef,
	"gitaly.LandResolution.tip":    isRef,

	"gitaly.LandResolvedFile.action":  notRef,
	"gitaly.LandResolvedFile.content": notRef,
	"gitaly.LandResolvedFile.mode":    notRef,
	"gitaly.LandResolvedFile.path":    notRef,

	"gitaly.ListAllBlobsResponse.Blob.data": notRef,
	"gitaly.ListAllBlobsResponse.Blob.oid":  isRef,

	"gitaly.ListBlobsRequest.revisions":  isRef,
	"gitaly.ListBlobsRequest.with_paths": notRef,

	"gitaly.ListBlobsResponse.Blob.data": notRef,
	"gitaly.ListBlobsResponse.Blob.oid":  isRef,
	"gitaly.ListBlobsResponse.Blob.path": notRef,

	"gitaly.ListBranchNamesContainingCommitRequest.commit_id": isRef,

	"gitaly.ListBranchNamesContainingCommitResponse.branch_names": isRef,

	"gitaly.ListCommitsByOidRequest.oid": isRef,

	"gitaly.ListCommitsByRefNameRequest.ref_names": isRef,

	"gitaly.ListCommitsByRefNameResponse.CommitForRef.ref_name": isRef,

	"gitaly.ListCommitsRequest.author":                  notRef,
	"gitaly.ListCommitsRequest.commit_message_patterns": notRef,
	"gitaly.ListCommitsRequest.disable_walk":            notRef,
	"gitaly.ListCommitsRequest.first_parent":            notRef,
	"gitaly.ListCommitsRequest.follow":                  notRef,
	"gitaly.ListCommitsRequest.ignore_case":             notRef,
	"gitaly.ListCommitsRequest.order":                   notRef,
	"gitaly.ListCommitsRequest.paths":                   notRef,
	"gitaly.ListCommitsRequest.reverse":                 notRef,
	"gitaly.ListCommitsRequest.revisions":               isRef,
	"gitaly.ListCommitsRequest.trailers":                notRef,

	"gitaly.ListFilesRequest.revision": isRef,

	"gitaly.ListFilesResponse.paths": notRef,

	"gitaly.ListLFSPointersRequest.revisions": isRef,

	"gitaly.ListLastCommitsForTreeRequest.literal_pathspec": notRef,
	"gitaly.ListLastCommitsForTreeRequest.path":             notRef,
	"gitaly.ListLastCommitsForTreeRequest.revision":         isRef,

	"gitaly.ListLastCommitsForTreeResponse.CommitForTree.path_bytes": notRef,

	"gitaly.ListRefsRequest.SortBy.direction": notRef,
	"gitaly.ListRefsRequest.SortBy.key":       notRef,

	"gitaly.ListRefsRequest.exclude_patterns":    notRef,
	"gitaly.ListRefsRequest.head":                notRef,
	"gitaly.ListRefsRequest.ignore_case":         notRef,
	"gitaly.ListRefsRequest.include_tagger_date": notRef,
	"gitaly.ListRefsRequest.patterns":            isRef,
	"gitaly.ListRefsRequest.peel_tags":           notRef,
	"gitaly.ListRefsRequest.pointing_at_oids":    isRef,

	"gitaly.ListRefsResponse.Reference.name":          isRef,
	"gitaly.ListRefsResponse.Reference.peeled_target": notRef,
	"gitaly.ListRefsResponse.Reference.target":        isRef,

	"gitaly.ListReviewsRequest.include_closed": notRef,
	"gitaly.ListReviewsRequest.target_branch":  notRef,

	"gitaly.ListTagNamesContainingCommitRequest.commit_id": isRef,

	"gitaly.ListTagNamesContainingCommitResponse.tag_names": isRef,

	"gitaly.OperationBranchUpdate.branch_created": notRef,
	"gitaly.OperationBranchUpdate.commit_id":      isRef,
	"gitaly.OperationBranchUpdate.repo_created":   notRef,

	"gitaly.PaginationCursor.next_cursor": notRef,

	"gitaly.PaginationParameter.page_token": notRef,

	"gitaly.PathHistoryRequest.path":     notRef,
	"gitaly.PathHistoryRequest.revision": isRef,

	"gitaly.Repository.git_alternate_object_directories": notRef,
	"gitaly.Repository.git_object_directory":             notRef,
	"gitaly.Repository.gl_project_path":                  notRef,
	"gitaly.Repository.gl_repository":                    notRef,
	"gitaly.Repository.relative_path":                    notRef,
	"gitaly.Repository.storage_name":                     notRef,

	"gitaly.RepositoryExistsResponse.exists": notRef,

	"gitaly.Review.approved":       notRef,
	"gitaly.Review.author":         notRef,
	"gitaly.Review.base_commit_id": isRef,
	"gitaly.Review.description":    notRef,
	"gitaly.Review.head_commit_id": isRef,
	"gitaly.Review.id":             notRef,
	"gitaly.Review.state":          notRef,
	"gitaly.Review.target_branch":  notRef,
	"gitaly.Review.title":          notRef,

	"gitaly.ReviewComment.actor":     notRef,
	"gitaly.ReviewComment.body":      notRef,
	"gitaly.ReviewComment.commit_id": isRef,
	"gitaly.ReviewComment.id":        notRef,
	"gitaly.ReviewComment.path":      notRef,

	"gitaly.ReviewFile.class":  notRef,
	"gitaly.ReviewFile.path":   notRef,
	"gitaly.ReviewFile.reason": notRef,

	"gitaly.ReviewRequirement.description": notRef,
	"gitaly.ReviewRequirement.paths":       notRef,
	"gitaly.ReviewRequirement.rule":        notRef,
	"gitaly.ReviewRequirement.satisfied":   notRef,

	"gitaly.ReviewVerdict.actor":     notRef,
	"gitaly.ReviewVerdict.body":      notRef,
	"gitaly.ReviewVerdict.commit_id": isRef,
	"gitaly.ReviewVerdict.current":   notRef,
	"gitaly.ReviewVerdict.kind":      notRef,

	"gitaly.SearchCodeRequest.case_sensitive": notRef,
	"gitaly.SearchCodeRequest.paths":          notRef,
	"gitaly.SearchCodeRequest.query":          notRef,
	"gitaly.SearchCodeRequest.regex":          notRef,
	"gitaly.SearchCodeRequest.revision":       isRef,

	"gitaly.SearchCodeResponse.Match.following_lines": notRef,
	"gitaly.SearchCodeResponse.Match.line":            notRef,
	"gitaly.SearchCodeResponse.Match.path":            notRef,
	"gitaly.SearchCodeResponse.Match.preceding_lines": notRef,

	"gitaly.SearchCodeResponse.commit_id": isRef,
	"gitaly.SearchCodeResponse.truncated": notRef,

	"gitaly.ServerInfoResponse.StorageStatus.filesystem_id": notRef,
	"gitaly.ServerInfoResponse.StorageStatus.fs_type":       notRef,
	"gitaly.ServerInfoResponse.StorageStatus.readable":      notRef,
	"gitaly.ServerInfoResponse.StorageStatus.storage_name":  notRef,
	"gitaly.ServerInfoResponse.StorageStatus.writeable":     notRef,

	"gitaly.ServerInfoResponse.git_version":    notRef,
	"gitaly.ServerInfoResponse.server_version": notRef,

	"gitaly.SubmitReviewVerdictRequest.body":           notRef,
	"gitaly.SubmitReviewVerdictRequest.head_commit_id": isRef,
	"gitaly.SubmitReviewVerdictRequest.id":             notRef,
	"gitaly.SubmitReviewVerdictRequest.kind":           notRef,

	"gitaly.Tag.id":             notRef,
	"gitaly.Tag.message":        notRef,
	"gitaly.Tag.name":           isRef,
	"gitaly.Tag.signature_type": notRef,

	"gitaly.TreeEntry.commit_oid": notRef,
	"gitaly.TreeEntry.flat_path":  notRef,
	"gitaly.TreeEntry.oid":        isRef,
	"gitaly.TreeEntry.path":       notRef,
	"gitaly.TreeEntry.type":       notRef,

	"gitaly.TreeEntryRequest.path":     notRef,
	"gitaly.TreeEntryRequest.revision": isRef,

	"gitaly.TreeEntryResponse.data": notRef,
	"gitaly.TreeEntryResponse.oid":  isRef,
	"gitaly.TreeEntryResponse.type": notRef,

	"gitaly.UpdateReviewRequest.expected_head_commit_id": isRef,
	"gitaly.UpdateReviewRequest.head_commit_id":          isRef,
	"gitaly.UpdateReviewRequest.id":                      notRef,

	"gitaly.User.email":       notRef,
	"gitaly.User.gl_id":       notRef,
	"gitaly.User.gl_username": notRef,
	"gitaly.User.name":        notRef,
	"gitaly.User.timezone":    notRef,

	"gitaly.UserCommitFilesAction.content": notRef,

	"gitaly.UserCommitFilesActionHeader.action":           notRef,
	"gitaly.UserCommitFilesActionHeader.base64_content":   notRef,
	"gitaly.UserCommitFilesActionHeader.execute_filemode": notRef,
	"gitaly.UserCommitFilesActionHeader.file_path":        notRef,
	"gitaly.UserCommitFilesActionHeader.infer_content":    notRef,
	"gitaly.UserCommitFilesActionHeader.previous_path":    notRef,

	"gitaly.UserCommitFilesRequestHeader.branch_name":         notRef,
	"gitaly.UserCommitFilesRequestHeader.commit_author_email": notRef,
	"gitaly.UserCommitFilesRequestHeader.commit_author_name":  notRef,
	"gitaly.UserCommitFilesRequestHeader.commit_message":      notRef,
	"gitaly.UserCommitFilesRequestHeader.expected_old_oid":    isRef,
	"gitaly.UserCommitFilesRequestHeader.force":               notRef,
	"gitaly.UserCommitFilesRequestHeader.sign":                notRef,
	"gitaly.UserCommitFilesRequestHeader.start_branch_name":   notRef,
	"gitaly.UserCommitFilesRequestHeader.start_sha":           isRef,

	"gitaly.UserCommitFilesResponse.index_error":       notRef,
	"gitaly.UserCommitFilesResponse.pre_receive_error": notRef,

	"gitaly.UserCreateBranchRequest.branch_name": notRef,
	"gitaly.UserCreateBranchRequest.start_point": isRef,

	"gitaly.UserDeleteBranchRequest.branch_name":      notRef,
	"gitaly.UserDeleteBranchRequest.expected_old_oid": isRef,

	"gitaly.UserLandRequest.branch":            notRef,
	"gitaly.UserLandRequest.commit_id":         isRef,
	"gitaly.UserLandRequest.create_branch":     notRef,
	"gitaly.UserLandRequest.merge_message":     notRef,
	"gitaly.UserLandRequest.resolve_conflicts": notRef,
	"gitaly.UserLandRequest.strategy":          notRef,

	"gitaly.UserLandResponse.already_landed":        notRef,
	"gitaly.UserLandResponse.already_landed_detail": notRef,
	"gitaly.UserLandResponse.landed_commit_id":      notRef,
	"gitaly.UserLandResponse.old_tip":               notRef,
	"gitaly.UserLandResponse.resolved_paths":        notRef,
	"gitaly.UserLandResponse.resolver":              notRef,
	"gitaly.UserLandResponse.reverted_by_commit_id": notRef,
	"gitaly.UserLandResponse.rewritten":             notRef,
}

// agentRefField returns what a field holds, and whether the exact table knows it.
func agentRefField(fd protoreflect.FieldDescriptor) (refFieldKind, bool) {
	if kind, ok := agentRefFields[fd.FullName()]; ok {
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
			kind, _ := agentRefField(fd)
			if kind != isRef && kind != isBranchName {
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
			if kind, _ := agentRefField(fd); kind != isRef && kind != isBranchName {
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
