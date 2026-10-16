package hook

import (
	"context"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
)

// StorageVoter is the write path of the Graft storage core
// (internal/graft/corestore/voter), called from the reference-transaction
// hook. See doc/graft/PLAN6.md, "Write path".
type StorageVoter interface {
	// Preparing does the slow work of the transaction (building and uploading
	// its pack) before git takes the reference locks. It never fails the
	// transaction: when it did not complete, Prepared does that work itself.
	Preparing(ctx context.Context, payload gitcmd.HooksPayload, changes []byte)
	// Prepared commits the transaction to the storage core while git holds
	// the reference locks. An error aborts the transaction.
	Prepared(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, meta changelog.Meta) error
	// Committed is called after git committed the transaction.
	Committed(ctx context.Context, payload gitcmd.HooksPayload, changes []byte)
	// Aborted is called after git aborted the transaction.
	Aborted(ctx context.Context, payload gitcmd.HooksPayload, changes []byte)
	// Authoritative reports whether the storage core's database replaces the
	// file changelog.
	Authoritative() bool
}

// WithStorageVoter makes the manager commit every reference transaction to
// the storage core.
func WithStorageVoter(voter StorageVoter) ManagerOption {
	return func(m *GitLabHookManager) {
		m.storageVoter = voter
	}
}

// storageAuthoritative reports whether the storage core's database is the
// source of truth, so that the file changelog is not written.
func (m *GitLabHookManager) storageAuthoritative() bool {
	return m.storageVoter != nil && m.storageVoter.Authoritative()
}

// storagePreparing hands a transaction to the storage core before git locks
// its references (doc/graft/PLAN6.md, LOCK AND HISTORY RULE). It never fails:
// the Preparing phase only moves work out of the locked Prepared phase. A
// transaction that the standalone access policy denies is aborted in the
// Prepared phase, so nothing is uploaded for it.
func (m *GitLabHookManager) storagePreparing(ctx context.Context, payload gitcmd.HooksPayload, env []string, changes []byte) {
	if m.storageVoter == nil {
		return
	}

	if message, err := m.graftPolicyDecision(env, changes); err != nil || message != "" {
		return
	}

	m.storageVoter.Preparing(ctx, payload, changes)
}

// storagePrepared hands a prepared transaction to the storage core.
func (m *GitLabHookManager) storagePrepared(ctx context.Context, payload gitcmd.HooksPayload, env []string, changes []byte) error {
	if m.storageVoter == nil {
		return nil
	}

	meta := changelog.Meta{Operation: changelog.UnknownOperation}
	if graftMeta, err := gitcmd.ChangelogMetaFromEnv(env); err == nil {
		meta = changelogMetaOf(payload, graftMeta)
	} else if payload.UserDetails != nil {
		meta.Actor = payload.UserDetails.UserID
	}

	return m.storageVoter.Prepared(ctx, payload, changes, meta)
}

// changelogMetaOf is the changelog metadata of a transaction, shared by the
// file changelog and the storage core.
func changelogMetaOf(payload gitcmd.HooksPayload, graftMeta gitcmd.ChangelogMeta) changelog.Meta {
	meta := changelog.Meta{
		Operation:        graftMeta.Operation,
		Actor:            graftMeta.Actor,
		Rewritten:        graftMeta.Rewritten,
		Members:          graftMeta.Members,
		MembersTruncated: graftMeta.MembersTruncated,
	}
	if meta.Operation == "" {
		meta.Operation = changelog.UnknownOperation
	}
	if meta.Actor == "" && payload.UserDetails != nil {
		meta.Actor = payload.UserDetails.UserID
	}
	return meta
}
