package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/remoterepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// unknownIndexError is an unspecified error that was produced by performing an invalid operation on the index.
type unknownIndexError string

// Error returns the error message of the unknown index error.
func (err unknownIndexError) Error() string { return string(err) }

// indexErrorType specifies which of the known index error types has occurred.
type indexErrorType uint

const (
	// ErrDirectoryExists represent a directory exists error.
	errDirectoryExists indexErrorType = iota
	// ErrDirectoryTraversal represent a directory traversal error.
	errDirectoryTraversal
	// ErrEmptyPath represent an empty path error.
	errEmptyPath
	// ErrFileExists represent a file exists error.
	errFileExists
	// ErrFileNotFound represent a file not found error.
	errFileNotFound
	// ErrInvalidPath represent an invalid path error.
	errInvalidPath
)

// IndexError is a well-defined error that was produced by performing an invalid operation on the index.
type indexError struct {
	path      string
	errorType indexErrorType
}

// Error returns the error message associated with the error type.
func (err indexError) Error() string {
	switch err.errorType {
	case errDirectoryExists:
		return "A directory with this name already exists"
	case errDirectoryTraversal:
		return "Path cannot include directory traversal"
	case errEmptyPath:
		return "You must provide a file path"
	case errFileExists:
		return "A file with this name already exists"
	case errFileNotFound:
		return "A file with this name doesn't exist"
	case errInvalidPath:
		return fmt.Sprintf("invalid path: %q", err.path)
	default:
		panic(fmt.Sprintf("unhandled IndexErrorType: %v", err.errorType))
	}
}

// Proto returns the Protobuf representation of this error.
func (err indexError) Proto() *gitalypb.IndexError {
	errType := gitalypb.IndexError_ERROR_TYPE_UNSPECIFIED
	switch err.errorType {
	case errDirectoryExists:
		errType = gitalypb.IndexError_ERROR_TYPE_DIRECTORY_EXISTS
	case errDirectoryTraversal:
		errType = gitalypb.IndexError_ERROR_TYPE_DIRECTORY_TRAVERSAL
	case errEmptyPath:
		errType = gitalypb.IndexError_ERROR_TYPE_EMPTY_PATH
	case errFileExists:
		errType = gitalypb.IndexError_ERROR_TYPE_FILE_EXISTS
	case errFileNotFound:
		errType = gitalypb.IndexError_ERROR_TYPE_FILE_NOT_FOUND
	case errInvalidPath:
		errType = gitalypb.IndexError_ERROR_TYPE_INVALID_PATH
	}

	return &gitalypb.IndexError{
		Path:      []byte(err.path),
		ErrorType: errType,
	}
}

// StructuredError returns the structured error.
func (err indexError) StructuredError() structerr.Error {
	e := errors.New(err.Error())
	switch err.errorType {
	case errDirectoryExists, errFileExists:
		return structerr.NewAlreadyExists("%w", e)
	case errDirectoryTraversal, errEmptyPath, errInvalidPath:
		return structerr.NewInvalidArgument("%w", e)
	case errFileNotFound:
		return structerr.NewNotFound("%w", e)
	default:
		return structerr.NewInternal("%w", e)
	}
}

// invalidArgumentError is returned when an invalid argument is provided.
type invalidArgumentError string

func (err invalidArgumentError) Error() string { return string(err) }

// UserCommitFiles allows for committing from a set of actions. See the protobuf documentation
// for details.
func (s *Server) UserCommitFiles(stream gitalypb.OperationService_UserCommitFilesServer) error {
	ctx := stream.Context()

	firstRequest, err := stream.Recv()
	if err != nil {
		return err
	}

	header := firstRequest.GetHeader()
	if header == nil {
		return structerr.NewInvalidArgument("empty UserCommitFilesRequestHeader")
	}

	if err := s.locator.ValidateRepository(ctx, header.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(header.GetRepository())

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	if err := validateUserCommitFilesHeader(header, objectHash); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	if err := s.userCommitFiles(ctx, header, stream, objectHash); err != nil {
		if fields := log.CustomFieldsFromContext(ctx); fields != nil {
			fields.RecordMetadata("repository_storage", header.GetRepository().GetStorageName())
			fields.RecordMetadata("repository_relative_path", header.GetRepository().GetRelativePath())
			fields.RecordMetadata("branch_name", header.GetBranchName())
			fields.RecordMetadata("start_branch_name", header.GetStartBranchName())
			fields.RecordMetadata("start_sha", header.GetStartSha())
			fields.RecordMetadata("force", header.GetForce())
		}

		if startRepo := header.GetStartRepository(); startRepo != nil {
			if fields := log.CustomFieldsFromContext(ctx); fields != nil {
				fields.RecordMetadata("start_repository_storage", startRepo.GetStorageName())
				fields.RecordMetadata("start_repository_relative_path", startRepo.GetRelativePath())
			}
		}

		var (
			unknownErr      unknownIndexError
			indexErr        indexError
			notAllowedError hook.NotAllowedError
			customHookErr   updateref.CustomHookError
		)

		switch {
		case errors.As(err, &unknownErr):
			// Problems that occur within git2go itself will still be returned
			// as UnknownIndexErrors. The most common case of this would be
			// creating an invalid path, e.g. '.git' but there are many other
			// potential, if unusual, issues that could occur.
			return unknownErr
		case errors.As(err, &indexErr):
			return indexErr.StructuredError().WithDetail(
				&gitalypb.UserCommitFilesError{
					Error: &gitalypb.UserCommitFilesError_IndexUpdate{
						IndexUpdate: indexErr.Proto(),
					},
				},
			)
		case errors.As(err, &notAllowedError):
			return structerr.NewPermissionDenied("denied by access checks: %w", err).WithDetail(
				&gitalypb.UserCommitFilesError{
					Error: &gitalypb.UserCommitFilesError_AccessCheck{
						AccessCheck: &gitalypb.AccessCheckError{
							ErrorMessage: notAllowedError.Message,
							UserId:       notAllowedError.UserID,
							Protocol:     notAllowedError.Protocol,
							Changes:      notAllowedError.Changes,
						},
					},
				},
			)
		case errors.As(err, &customHookErr):
			return structerr.NewPermissionDenied("denied by custom hooks: %w", err).WithDetail(
				&gitalypb.UserCommitFilesError{
					Error: &gitalypb.UserCommitFilesError_CustomHook{
						CustomHook: customHookErr.Proto(),
					},
				},
			)
		case errors.As(err, new(invalidArgumentError)):
			return structerr.NewInvalidArgument("%w", err)
		default:
			return err
		}
	}

	return nil
}

func validatePath(rootPath, relPath string) (string, error) {
	if relPath == "" {
		return "", indexError{errorType: errEmptyPath}
	} else if strings.Contains(relPath, "//") {
		// This is a workaround to address a quirk in porting the RPC from Ruby to Go.
		// GitLab's QA pipeline runs tests with filepath 'invalid://file/name/here'.
		// Go's filepath.Clean returns 'invalid:/file/name/here'. The Ruby implementation's
		// filepath normalization accepted the path as is. Adding a file with this path to the
		// index via Rugged failed with an invalid path error. As Go's cleaning resulted a valid
		// filepath, adding the file succeeded, which made the QA pipeline's specs fail.
		//
		// The Rails code expects to receive an error prefixed with 'invalid path', which is done
		// here to retain compatibility.
		return "", indexError{errorType: errInvalidPath, path: relPath}
	}

	path, err := storage.ValidateRelativePath(rootPath, relPath)
	if err != nil {
		if errors.Is(err, storage.ErrRelativePathEscapesRoot) {
			return "", indexError{errorType: errDirectoryTraversal, path: relPath}
		}

		return "", err
	}

	return path, nil
}

// applyAction applies an action to an TreeEntry.
func applyAction(
	ctx context.Context,
	action commitAction,
	root *localrepo.TreeEntry,
	repo *localrepo.Repo,
) error {
	switch action := action.(type) {
	case changeFileMode:
		if err := root.Modify(
			action.Path,
			func(entry *localrepo.TreeEntry) error {
				// Only files have an executable mode. A tree or a submodule given a file's mode
				// would make a tree that git refuses, or one whose entry is no longer a
				// submodule.
				if entry.Type != localrepo.Blob {
					return indexError{
						path:      action.Path,
						errorType: errFileNotFound,
					}
				}

				if action.ExecutableMode {
					if entry.Mode != "100755" {
						entry.Mode = "100755"
					}
				} else {
					if entry.Mode == "100755" {
						entry.Mode = "100644"
					}
				}

				return nil
			}); err != nil {
			return translateError(err, action.Path)
		}
	case updateFile:
		if err := root.Modify(
			action.Path,
			func(entry *localrepo.TreeEntry) error {
				// Only blobs can be updated. Updating a tree or submodule entry would
				// overwrite its OID with a blob while keeping its tree mode/type, corrupting the repository.
				if entry.Type != localrepo.Blob {
					return indexError{
						path:      action.Path,
						errorType: errFileNotFound,
					}
				}

				entry.OID = git.ObjectID(action.OID)
				return nil
			}); err != nil {
			return translateError(err, action.Path)
		}
	case moveFile:
		entry, err := root.Get(action.Path)
		if err != nil {
			return translateError(err, action.Path)
		}

		if entry.Type != localrepo.Blob {
			return indexError{
				path:      action.Path,
				errorType: errFileNotFound,
			}
		}

		mode := entry.Mode

		if action.OID == "" {
			action.OID = string(entry.OID)
		}

		if err := root.Delete(action.Path); err != nil {
			return translateError(err, action.Path)
		}

		if err := root.Add(
			action.NewPath,
			localrepo.TreeEntry{
				OID:  git.ObjectID(action.OID),
				Mode: mode,
				Path: filepath.Base(action.NewPath),
				// The moved entry is a file: a later action of the request sees it as one.
				Type: entry.Type,
			},
			localrepo.WithOverwriteDirectory(),
		); err != nil {
			return translateError(err, action.NewPath)
		}
	case createDirectory:
		if entry, err := root.Get(action.Path); err != nil && !errors.Is(err, localrepo.ErrEntryNotFound) {
			return translateError(err, action.Path)
		} else if entry != nil {
			switch entry.Type {
			case localrepo.Tree, localrepo.Submodule:
				return indexError{
					path:      action.Path,
					errorType: errDirectoryExists,
				}
			default:
				return indexError{
					path:      action.Path,
					errorType: errFileExists,
				}
			}
		}

		blobID, err := repo.WriteBlob(ctx, strings.NewReader(""), localrepo.WriteBlobConfig{
			Path: filepath.Join(action.Path, ".gitkeep"),
		})
		if err != nil {
			return err
		}

		if err := root.Add(
			filepath.Join(action.Path, ".gitkeep"),
			localrepo.TreeEntry{
				Mode: "100644",
				Path: ".gitkeep",
				Type: localrepo.Blob,
				OID:  blobID,
			},
		); err != nil {
			if errors.Is(err, localrepo.ErrEntryExists) {
				return indexError{
					path:      action.Path,
					errorType: errDirectoryExists,
				}
			}

			return translateError(err, action.Path)
		}
	case createFile:
		mode := "100644"
		if action.ExecutableMode {
			mode = "100755"
		}

		if err := root.Add(
			action.Path,
			localrepo.TreeEntry{
				OID:  git.ObjectID(action.OID),
				Path: filepath.Base(action.Path),
				Type: localrepo.Blob,
				Mode: mode,
			},
			localrepo.WithOverwriteDirectory(),
		); err != nil {
			return translateError(err, action.Path)
		}
	case deleteFile:
		if err := root.Delete(
			action.Path,
		); err != nil {
			return translateError(err, action.Path)
		}
	default:
		return errors.New("unsupported action")
	}

	return nil
}

// translateLocalrepoError converts errors returned by the `localrepo` package into nice errors that we can return to the caller.
// Most importantly, these errors will carry metadata that helps to figure out what exactly has gone wrong.
func translateError(err error, path string) error {
	// A file in the way of the path: the error names that file, which already exists.
	var fileInPath localrepo.FileInPathError
	if errors.As(err, &fileInPath) {
		return indexError{
			path:      fileInPath.Path,
			errorType: errFileExists,
		}
	}

	switch {
	case errors.Is(err, localrepo.ErrEntryNotFound) ||
		errors.Is(err, localrepo.ErrObjectNotFound):
		return indexError{
			path:      path,
			errorType: errFileNotFound,
		}
	case errors.Is(err, localrepo.ErrEmptyPath) ||
		errors.Is(err, localrepo.ErrPathTraversal) ||
		errors.Is(err, localrepo.ErrAbsolutePath) ||
		errors.Is(err, localrepo.ErrDisallowedPath):
		//The error coming back from git2go has the path in single
		//quotes. This is to match the git2go error for now.
		//nolint:gitaly-linters
		return unknownIndexError(
			fmt.Sprintf("invalid path: '%s'", path),
		)
	case errors.Is(err, localrepo.ErrPathTraversal):
		return indexError{
			path:      path,
			errorType: errDirectoryTraversal,
		}
	case errors.Is(err, localrepo.ErrEntryExists):
		return indexError{
			path:      path,
			errorType: errFileExists,
		}
	}
	return err
}

var errSignatureMissingNameOrEmail = errors.New(
	"commit: failed to parse signature - Signature cannot have an empty name or email",
)

func (s *Server) userCommitFilesGit(
	ctx context.Context,
	header *gitalypb.UserCommitFilesRequestHeader,
	parentCommitOID git.ObjectID,
	quarantineRepo *localrepo.Repo,
	reader catfile.ObjectReader,
	repoPath string,
	actions []commitAction,
) (git.ObjectID, error) {
	committerSignature, err := git.SignatureFromRequest(header)
	if err != nil {
		return "", structerr.NewInvalidArgument("%w", err)
	}

	var treeish git.ObjectID

	if parentCommitOID != "" {
		treeish, err = quarantineRepo.ResolveRevisionWithReader(
			ctx,
			reader,
			git.Revision(fmt.Sprintf("%s^{tree}", parentCommitOID)),
		)
		if err != nil {
			return "", fmt.Errorf("getting tree id: %w", err)
		}
	}

	treeEntry := &localrepo.TreeEntry{
		Mode: "040000",
		Type: localrepo.Tree,
	}

	if treeish != "" {
		// Only the trees the actions descend into are read.
		treeEntry, err = quarantineRepo.ReadTreeLazily(ctx, reader, treeish)
		if err != nil {
			return "", fmt.Errorf("reading tree: %w", err)
		}
	}

	for _, action := range actions {
		if err = applyAction(
			ctx,
			action,
			treeEntry,
			quarantineRepo,
		); err != nil {
			return "", fmt.Errorf("performing action %T: %w", action, err)
		}
	}

	if err := writeTreeEntries(ctx, quarantineRepo, treeEntry); err != nil {
		return "", fmt.Errorf("writing tree %w", err)
	}

	treeish = treeEntry.OID

	if treeish == "" {
		objectHash, err := quarantineRepo.ObjectHash(ctx)
		if err != nil {
			return "", fmt.Errorf("getting object hash: %w", err)
		}

		treeish = objectHash.EmptyTreeOID
	}

	cfg := localrepo.WriteCommitConfig{
		AuthorDate:     committerSignature.When,
		AuthorName:     strings.TrimSpace(string(header.GetCommitAuthorName())),
		AuthorEmail:    strings.TrimSpace(string(header.GetCommitAuthorEmail())),
		CommitterDate:  committerSignature.When,
		CommitterName:  committerSignature.Name,
		CommitterEmail: committerSignature.Email,
		Message:        string(header.GetCommitMessage()),
		TreeID:         treeish,
		GitConfig:      s.gitConfig,
		Sign:           header.GetSign(),
	}

	if cfg.AuthorName == "" {
		cfg.AuthorName = cfg.CommitterName
	}

	if cfg.AuthorEmail == "" {
		cfg.AuthorEmail = cfg.CommitterEmail
	}

	if cfg.AuthorName == "" || cfg.AuthorEmail == "" {
		return "", structerr.NewInvalidArgument("%w", errSignatureMissingNameOrEmail)
	}

	if parentCommitOID != "" {
		cfg.Parents = []git.ObjectID{parentCommitOID}
	}

	return quarantineRepo.WriteCommit(
		ctx,
		cfg,
	)
}

// writeTreeEntries writes the trees of tree that changed through one tree writer, which it closes
// on every path: once it returns, every tree it wrote is in the object directory.
func writeTreeEntries(ctx context.Context, repo *localrepo.Repo, tree *localrepo.TreeEntry) (returnedErr error) {
	treeWriter, err := repo.NewTreeWriter(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := treeWriter.Close(); err != nil && returnedErr == nil {
			returnedErr = err
		}
	}()

	return tree.WriteWith(treeWriter)
}

func (s *Server) userCommitFiles(
	ctx context.Context,
	header *gitalypb.UserCommitFilesRequestHeader,
	stream gitalypb.OperationService_UserCommitFilesServer,
	objectHash git.ObjectHash,
) error {
	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, header.GetRepository())
	if err != nil {
		return err
	}
	defer cleanup()

	repoPath, err := quarantineRepo.Path(ctx)
	if err != nil {
		return err
	}

	remoteRepo := header.GetStartRepository()
	if sameRepository(header.GetRepository(), remoteRepo) {
		// Some requests set a StartRepository that refers to the same repository as the target repository.
		// This check never works behind Praefect. See: https://gitlab.com/gitlab-org/gitaly/-/issues/3294
		// Plain Gitalies still benefit from identifying the case and avoiding unnecessary RPC to resolve the
		// branch.
		remoteRepo = nil
	}

	// One object reader serves every revision and tree the request reads (PLAN9, WP96). It reads
	// the quarantined repository, so it sees the objects the request writes.
	reader, cancelReader, err := quarantineRepo.RequestObjectReader(ctx)
	if err != nil {
		return err
	}
	defer cancelReader()
	if s.testHook != nil {
		s.testHook("UserCommitFiles: reader taken")
	}

	// The branches are resolved by their exact names: a missing branch never resolves through a
	// tag or another reference that git's DWIM rules would try after it.
	targetBranchName := git.NewReferenceNameFromBranchName(string(header.GetBranchName()))
	branchNames := []git.ReferenceName{targetBranchName}
	var startBranchName git.ReferenceName
	if header.GetStartSha() == "" && remoteRepo == nil && len(header.GetStartBranchName()) > 0 {
		startBranchName = git.NewReferenceNameFromBranchName(string(header.GetStartBranchName()))
		branchNames = append(branchNames, startBranchName)
	}
	branches, err := quarantineRepo.ResolveBranchesExactly(ctx, reader, branchNames...)
	if err != nil {
		return fmt.Errorf("resolve branches: %w", err)
	}
	// Empty when the branch is being created.
	targetBranchCommit := branches[targetBranchName]

	var parentCommitOID git.ObjectID
	// startBranchResolved is whether the start branch resolved in the repository, which then has
	// a branch.
	var startBranchResolved bool
	if header.GetStartSha() == "" {
		parentCommitOID, startBranchResolved, err = s.resolveParentCommit(
			ctx,
			quarantineRepo,
			branches,
			remoteRepo,
			targetBranchName,
			targetBranchCommit,
			startBranchName,
			string(header.GetStartBranchName()),
		)
		if err != nil {
			return fmt.Errorf("resolve parent commit: %w", err)
		}
	} else {
		parentCommitOID, err = objectHash.FromHex(header.GetStartSha())
		if err != nil {
			return structerr.NewInvalidArgument("cannot resolve parent commit: %w", err)
		}
	}

	if parentCommitOID != targetBranchCommit {
		if err := s.fetchMissingCommit(ctx, quarantineRepo, reader, remoteRepo, parentCommitOID); err != nil {
			return fmt.Errorf("fetch missing commit: %w", err)
		}
	}

	type action struct {
		header  *gitalypb.UserCommitFilesActionHeader
		content []byte
	}

	var pbActions []action

	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fmt.Errorf("receive request: %w", err)
		}

		switch payload := req.GetAction().GetUserCommitFilesActionPayload().(type) {
		case *gitalypb.UserCommitFilesAction_Header:
			pbActions = append(pbActions, action{header: payload.Header})
		case *gitalypb.UserCommitFilesAction_Content:
			if len(pbActions) == 0 {
				return errors.New("content sent before action")
			}

			// append the content to the previous action
			content := &pbActions[len(pbActions)-1].content
			*content = append(*content, payload.Content...)
		default:
			return fmt.Errorf("unhandled action payload type: %T", payload)
		}
	}

	actions := make([]commitAction, 0, len(pbActions))
	for _, pbAction := range pbActions {
		if _, ok := gitalypb.UserCommitFilesActionHeader_ActionType_name[int32(pbAction.header.GetAction())]; !ok {
			return structerr.NewInvalidArgument("NoMethodError: undefined method `downcase' for %d:Integer", pbAction.header.GetAction())
		}

		path, err := validatePath(repoPath, string(pbAction.header.GetFilePath()))
		if err != nil {
			return structerr.NewInvalidArgument("validate path: %w", err)
		}

		content := io.Reader(bytes.NewReader(pbAction.content))
		if pbAction.header.GetBase64Content() {
			content = base64.NewDecoder(base64.StdEncoding, content)
		}

		switch pbAction.header.GetAction() {
		case gitalypb.UserCommitFilesActionHeader_CREATE:
			blobID, err := quarantineRepo.WriteBlob(ctx, content, localrepo.WriteBlobConfig{
				Path: path,
			})
			if err != nil {
				return fmt.Errorf("write created blob: %w", err)
			}

			actions = append(actions, createFile{
				OID:            blobID.String(),
				Path:           path,
				ExecutableMode: pbAction.header.GetExecuteFilemode(),
			})
		case gitalypb.UserCommitFilesActionHeader_CHMOD:
			actions = append(actions, changeFileMode{
				Path:           path,
				ExecutableMode: pbAction.header.GetExecuteFilemode(),
			})
		case gitalypb.UserCommitFilesActionHeader_MOVE:
			prevPath, err := validatePath(repoPath, string(pbAction.header.GetPreviousPath()))
			if err != nil {
				return structerr.NewInvalidArgument("validate previous path: %w", err)
			}

			var oid git.ObjectID
			if !pbAction.header.GetInferContent() {
				var err error
				oid, err = quarantineRepo.WriteBlob(ctx, content, localrepo.WriteBlobConfig{
					Path: path,
				})
				if err != nil {
					return err
				}
			}
			actions = append(actions, moveFile{
				Path:    prevPath,
				NewPath: path,
				OID:     oid.String(),
			})
		case gitalypb.UserCommitFilesActionHeader_UPDATE:
			oid, err := quarantineRepo.WriteBlob(ctx, content, localrepo.WriteBlobConfig{
				Path: path,
			})
			if err != nil {
				return fmt.Errorf("write updated blob: %w", err)
			}

			actions = append(actions, updateFile{
				Path: path,
				OID:  oid.String(),
			})
		case gitalypb.UserCommitFilesActionHeader_DELETE:
			actions = append(actions, deleteFile{
				Path: path,
			})
		case gitalypb.UserCommitFilesActionHeader_CREATE_DIR:
			actions = append(actions, createDirectory{
				Path: path,
			})
		}
	}

	commitID, err := s.userCommitFilesGit(
		ctx,
		header,
		parentCommitOID,
		quarantineRepo,
		reader,
		repoPath,
		actions,
	)
	if err != nil {
		if errors.Is(err, localrepo.ErrDisallowedCharacters) {
			return structerr.NewInvalidArgument("%w", errSignatureMissingNameOrEmail)
		}

		return err
	}

	// A branch that resolved already tells that the repository has branches.
	hasBranches := targetBranchCommit != "" || startBranchResolved
	if !hasBranches {
		hasBranches, err = quarantineRepo.HasBranches(ctx)
		if err != nil {
			return fmt.Errorf("was repo created: %w", err)
		}
	}

	var oldRevision git.ObjectID
	if expectedOldOID := header.GetExpectedOldOid(); expectedOldOID != "" {
		oldRevision, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return structerr.NewInvalidArgument("invalid expected old object ID: %w", err).WithMetadata("old_object_id", expectedOldOID)
		}

		oldRevision, err = resolveRevision(ctx, s.localRepoFactory.Build(header.GetRepository()), oldRevision)
		if err != nil {
			return structerr.NewInvalidArgument("cannot resolve expected old object ID: %w", err).
				WithMetadata("old_object_id", expectedOldOID)
		}
	} else {
		oldRevision = parentCommitOID
		if targetBranchCommit == "" {
			oldRevision = objectHash.ZeroOID
		} else if header.GetForce() {
			oldRevision = targetBranchCommit
		}
	}

	if err := s.updateReferenceWithHooks(ctx, header.GetRepository(), header.GetUser(), quarantineDir, targetBranchName, commitID, oldRevision); err != nil {
		if errors.As(err, &updateref.Error{}) {
			return structerr.NewFailedPrecondition("%w", err)
		}

		return fmt.Errorf("update reference: %w", err)
	}

	return stream.SendAndClose(&gitalypb.UserCommitFilesResponse{BranchUpdate: &gitalypb.OperationBranchUpdate{
		CommitId:      commitID.String(),
		RepoCreated:   !hasBranches,
		BranchCreated: objectHash.IsZeroOID(oldRevision),
	}})
}

func sameRepository(repoA, repoB *gitalypb.Repository) bool {
	return repoA.GetStorageName() == repoB.GetStorageName() &&
		repoA.GetRelativePath() == repoB.GetRelativePath()
}

// resolveParentCommit returns the parent commit of the new commit, and whether it is the start
// branch of local, which then has a branch. localBranches holds the commits of the target and
// start branches of local that exist under their exact names.
func (s *Server) resolveParentCommit(
	ctx context.Context,
	local *localrepo.Repo,
	localBranches map[git.ReferenceName]git.ObjectID,
	remote *gitalypb.Repository,
	targetBranch git.ReferenceName,
	targetBranchCommit git.ObjectID,
	localStartBranch git.ReferenceName,
	startBranch string,
) (git.ObjectID, bool, error) {
	if remote == nil && startBranch == "" {
		return targetBranchCommit, false, nil
	}

	branch := targetBranch
	if startBranch != "" {
		branch = git.NewReferenceNameFromBranchName(startBranch)
	}
	refish := branch + "^{commit}"

	if remote == nil {
		if commit, ok := localBranches[localStartBranch]; ok {
			return commit, true, nil
		}

		// A repository whose target branch exists has branches: the workaround below matters
		// only when it does not.
		hasBranches := targetBranchCommit != ""
		if !hasBranches {
			var err error
			if hasBranches, err = local.HasBranches(ctx); err != nil {
				return "", false, fmt.Errorf("has branches: %w", err)
			}
		}
		if !hasBranches {
			// The first branch is being created: see the workaround below.
			return targetBranchCommit, false, nil
		}

		return "", false, fmt.Errorf("resolving refish %q in %T: %w", refish, local, git.ErrReferenceNotFound)
	}

	repo, err := remoterepo.New(ctx, remote, s.conns)
	if err != nil {
		return "", false, fmt.Errorf("remote repository: %w", err)
	}

	if hasBranches, err := repo.HasBranches(ctx); err != nil {
		return "", false, fmt.Errorf("has branches: %w", err)
	} else if !hasBranches {
		// GitLab sends requests to UserCommitFiles where target repository
		// and start repository are the same. If the request hits Gitaly directly,
		// Gitaly could check if the repos are the same by comparing their storages
		// and relative paths and simply resolve the branch locally. When request is proxied
		// through Praefect, the start repository's storage is not rewritten, thus Gitaly can't
		// identify the repos as being the same.
		//
		// If the start repository is set, we have to resolve the branch there as it
		// might be on a different commit than the local repository. As Gitaly can't identify
		// the repositories are the same behind Praefect, it has to perform an RPC to resolve
		// the branch. The resolving would fail as the branch does not yet exist in the start
		// repository, which is actually the local repository.
		//
		// Due to this, we check if the remote has any branches. If not, we likely hit this case
		// and we're creating the first branch. If so, we'll just return the commit that was
		// already resolved locally.
		//
		// See: https://gitlab.com/gitlab-org/gitaly/-/issues/3294
		return targetBranchCommit, false, nil
	}

	// By its exact name, as in the local repository: a missing branch does not resolve through
	// a tag or another reference of the start repository.
	commit, err := repo.ResolveBranch(ctx, strings.TrimPrefix(branch.String(), "refs/heads/"))
	if err != nil {
		return "", false, fmt.Errorf("resolving refish %q in %T: %w", refish, repo, err)
	}

	return commit, false, nil
}

func (s *Server) fetchMissingCommit(
	ctx context.Context,
	localRepo *localrepo.Repo,
	localReader catfile.ObjectInfoReader,
	remoteRepo *gitalypb.Repository,
	commit git.ObjectID,
) error {
	if _, err := localRepo.ResolveRevisionWithReader(ctx, localReader, commit.Revision()+"^{commit}"); err != nil {
		if !errors.Is(err, git.ErrReferenceNotFound) || remoteRepo == nil {
			return fmt.Errorf("lookup parent commit: %w", err)
		}

		if err := localRepo.FetchInternal(
			ctx,
			remoteRepo,
			[]string{commit.String()},
			localrepo.FetchOpts{Tags: localrepo.FetchOptsTagsNone},
		); err != nil {
			return fmt.Errorf("fetch parent commit: %w", err)
		}
	}

	return nil
}

func validateUserCommitFilesHeader(header *gitalypb.UserCommitFilesRequestHeader, objectHash git.ObjectHash) error {
	if header.GetUser() == nil {
		return errors.New("empty User")
	}
	if len(header.GetCommitMessage()) == 0 {
		return errors.New("empty CommitMessage")
	}
	if len(header.GetBranchName()) == 0 {
		return errors.New("empty BranchName")
	}

	startSha := header.GetStartSha()
	if len(startSha) > 0 {
		err := objectHash.ValidateHex(startSha)
		if err != nil {
			return err
		}
	}

	return nil
}

// commitAction represents an action taken to build a commit.
type commitAction interface{ action() }

// isAction is used ensuring type safety for actions.
type isAction struct{}

func (isAction) action() {}

// changeFileMode sets a file's mode to either regular or executable file.
// FileNotFoundError is returned when attempting to change a non-existent
// file's mode.
type changeFileMode struct {
	isAction
	// Path is the path of the whose mode to change.
	Path string
	// ExecutableMode indicates whether the file mode should be changed to executable or not.
	ExecutableMode bool
}

// createDirectory creates a directory in the given path with a '.gitkeep' file inside.
// FileExistsError is returned if a file already exists at the provided path.
// DirectoryExistsError is returned if a directory already exists at the provided
// path.
type createDirectory struct {
	isAction
	// Path is the path of the directory to create.
	Path string
}

// createFile creates a file using the provided path, mode and oid as the blob.
// FileExistsError is returned if a file exists at the given path.
type createFile struct {
	isAction
	// Path is the path of the file to create.
	Path string
	// ExecutableMode indicates whether the file mode should be executable or not.
	ExecutableMode bool
	// OID is the id of the object that contains the content of the file.
	OID string
}

// deleteFile deletes a file or a directory from the provided path.
// FileNotFoundError is returned if the file does not exist.
type deleteFile struct {
	isAction
	// Path is the path of the file to delete.
	Path string
}

// moveFile moves a file or a directory to the new path.
// FileNotFoundError is returned if the file does not exist.
type moveFile struct {
	isAction
	// Path is the path of the file to move.
	Path string
	// NewPath is the new path of the file.
	NewPath string
	// OID is the id of the object that contains the content of the file. If set,
	// the file contents are updated to match the object, otherwise the file keeps
	// the existing content.
	OID string
}

// updateFile updates a file at the given path to point to the provided
// OID. FileNotFoundError is returned if the file does not exist.
type updateFile struct {
	isAction
	// Path is the path of the file to update.
	Path string
	// OID is the id of the object that contains the new content of the file.
	OID string
}
