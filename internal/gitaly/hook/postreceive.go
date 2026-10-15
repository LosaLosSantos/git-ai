package hook

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

const (
	// A standard terminal window is (at least) 80 characters wide.
	terminalWidth                = 80
	gitRemoteMessagePrefixLength = len("remote: ")
	terminalMessagePadding       = 2

	// Git prefixes remote messages with "remote: ", so this width is subtracted
	// from the width available to us.
	maxMessageWidth = terminalWidth - gitRemoteMessagePrefixLength

	// Our centered text shouldn't start or end right at the edge of the window,
	// so we add some horizontal padding: 2 chars on either side.
	maxMessageTextWidth = maxMessageWidth - 2*terminalMessagePadding
)

func printMessages(messages []gitlab.PostReceiveMessage, w io.Writer) error {
	for _, message := range messages {
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}

		switch message.Type {
		case "basic":
			if _, err := w.Write([]byte(message.Message)); err != nil {
				return err
			}
		case "alert":
			if err := printAlert(message, w); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid message type: %v", message.Type)
		}

		if _, err := w.Write([]byte("\n\n")); err != nil {
			return err
		}
	}

	return nil
}

func centerLine(b []byte) []byte {
	b = bytes.TrimSpace(b)
	linePadding := int(math.Max((float64(maxMessageWidth)-float64(len(b)))/2, 0))
	return append(bytes.Repeat([]byte(" "), linePadding), b...)
}

func printAlert(m gitlab.PostReceiveMessage, w io.Writer) error {
	if _, err := w.Write(bytes.Repeat([]byte("="), maxMessageWidth)); err != nil {
		return err
	}

	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}

	words := strings.Fields(m.Message)

	line := bytes.NewBufferString("")

	for _, word := range words {
		if line.Len()+1+len(word) > maxMessageTextWidth {
			if _, err := w.Write(append(centerLine(line.Bytes()), '\n')); err != nil {
				return err
			}
			line.Reset()
		}

		if _, err := line.WriteString(word + " "); err != nil {
			return err
		}
	}

	if _, err := w.Write(centerLine(line.Bytes())); err != nil {
		return err
	}

	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}

	if _, err := w.Write(bytes.Repeat([]byte("="), maxMessageWidth)); err != nil {
		return err
	}

	return nil
}

//nolint:revive // This is unintentionally missing documentation.
func (m *GitLabHookManager) PostReceiveHook(ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	payload, err := gitcmd.HooksPayloadFromEnv(env)
	if err != nil {
		return structerr.NewInternal("extracting hooks payload: %w", err)
	}

	changes, err := io.ReadAll(stdin)
	if err != nil {
		return structerr.NewInternal("reading stdin from request: %w", err)
	}

	stats := countRefUpdates(changes)

	// Record the per-type reference-update counts as Prometheus counters so the
	// churn is graphable with rate() and alertable. The overall total is derived
	// in Prometheus with sum(rate(...)), so it doesn't need its own series.
	m.refUpdatesCounter.WithLabelValues(refUpdateCreated).Add(float64(stats.created))
	m.refUpdatesCounter.WithLabelValues(refUpdateUpdated).Add(float64(stats.updated))
	m.refUpdatesCounter.WithLabelValues(refUpdateDeleted).Add(float64(stats.deleted))

	// Record the counts in customFields which will be propagated to the post-receive hook's logger.
	if customFields := log.CustomFieldsFromContext(ctx); customFields != nil {
		customFields.RecordMetadata("post_receive_pack.ref_updates.total", stats.total)
		customFields.RecordMetadata("post_receive_pack.ref_updates.created", stats.created)
		customFields.RecordMetadata("post_receive_pack.ref_updates.deleted", stats.deleted)
		customFields.RecordMetadata("post_receive_pack.ref_updates.updated", stats.updated)
	}

	if isPrimary(payload) {
		// Wait for Praefect to signal that transaction coordination is
		// complete before calling the post receive hook. This prevents
		// a race condition where Rails is notified before all replicas
		// have applied the write and before Praefect has updated its
		// database for routing.
		if payload.Transaction != nil && payload.Transaction.BackchannelID != 0 &&
			featureflag.PraefectPostReceiveSignal.IsEnabled(ctx) {
			if err := m.waitForPraefectSignal(ctx, payload.Transaction.ID); err != nil {
				// If the signal carries a DB write failure, return an error so that
				// git-receive-pack exits non-zero and Rails is not notified of a
				// write that is not yet reflected in Praefect's routing state.
				// On timeout or context cancellation, also fail rather than
				// proceeding with potentially inconsistent state.
				return fmt.Errorf("waiting for Praefect signal: %w", err)
			}
		}

		if err := m.postReceiveHook(ctx, payload, repo, pushOptions, env, changes, stdout, stderr); err != nil {
			m.logger.WithError(err).WarnContext(ctx, "stopping transaction because post-receive hook failed")

			// If the post-receive hook declines the push, then we need to stop any
			// secondaries voting on the transaction.
			if err := m.stopTransaction(ctx, payload); err != nil {
				m.logger.WithError(err).ErrorContext(ctx, "failed stopping transaction in post-receive hook")
			}

			return err
		}
	}

	return nil
}

func (m *GitLabHookManager) postReceiveHook(ctx context.Context, payload gitcmd.HooksPayload, repo *gitalypb.Repository, pushOptions, env []string, stdin []byte, stdout, stderr io.Writer) error {
	if len(stdin) == 0 {
		return structerr.NewInternal("hook got no reference updates")
	}

	if payload.UserDetails == nil {
		return structerr.NewInternal("payload has no receive hooks info")
	}
	if payload.UserDetails.UserID == "" {
		return structerr.NewInternal("user ID not set")
	}
	if repo.GetGlRepository() == "" {
		return structerr.NewInternal("repository not set")
	}

	ok, messages, err := m.gitlabClient.PostReceive(
		ctx, repo.GetGlRepository(),
		payload.UserDetails.UserID,
		string(stdin),
		payload.GitalyClientContext,
		pushOptions...,
	)
	if err != nil {
		return fmt.Errorf("GitLab: %w", err)
	}

	if err := printMessages(messages, stdout); err != nil {
		return fmt.Errorf("error writing messages to stream: %w", err)
	}

	if !ok {
		return errors.New("")
	}

	executor, err := m.newCustomHooksExecutor(ctx, repo, "post-receive")
	if err != nil {
		return structerr.NewInternal("creating custom hooks executor: %w", err)
	}

	customEnv, err := m.customHooksEnv(ctx, payload, pushOptions, env)
	if err != nil {
		return structerr.NewInternal("constructing custom hook environment: %w", err)
	}

	if err = executor(
		ctx,
		nil,
		customEnv,
		bytes.NewReader(stdin),
		stdout,
		stderr,
	); err != nil {
		return fmt.Errorf("executing custom hooks: %w", err)
	}

	return nil
}

// refUpdateStats summarizes the reference updates contained in a post-receive
// hook's stdin.
type refUpdateStats struct {
	total, created, deleted, updated int
}

// countRefUpdates counts the reference updates from the post-receive hook's
// stdin. Each line has the format "<oldOID> <newOID> <refname>". A reference
// is considered created when its old OID is all-zeroes and deleted when its new
// OID is all-zeroes; everything else is an update.
func countRefUpdates(changes []byte) refUpdateStats {
	var stats refUpdateStats

	scanner := bufio.NewScanner(bytes.NewReader(changes))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}

		stats.total++

		oldZero := isZeroOID(fields[0])
		newZero := isZeroOID(fields[1])
		switch {
		case oldZero && !newZero:
			stats.created++
		case !oldZero && newZero:
			stats.deleted++
		default:
			stats.updated++
		}
	}

	return stats
}

// isZeroOID reports whether the given object ID consists only of zeroes, which
// git uses to denote a missing reference. This works for both SHA-1 and SHA-256
// object IDs without needing to know the object hash.
func isZeroOID(oid string) bool {
	return oid != "" && strings.Trim(oid, "0") == ""
}

// waitForPraefectSignal waits for Praefect to signal that transaction coordination
// is complete. This is called on the primary node before executing the post-receive
// hook to ensure all replicas have applied the write and Praefect has updated its
// database for routing decisions.
func (m *GitLabHookManager) waitForPraefectSignal(ctx context.Context, txID uint64) error {
	if m.postReceiveRegistry == nil {
		return nil
	}

	waitCh, cleanup, err := m.postReceiveRegistry.RegisterWaiter(txID)
	if err != nil {
		return fmt.Errorf("registering waiter: %w", err)
	}
	defer cleanup()

	waitTimer := m.praefectSignalTicker(m.postReceiveWaitTimeout)
	waitTimer.Reset()
	defer waitTimer.Stop()

	select {
	case signalErr := <-waitCh:
		return signalErr
	case <-waitTimer.C():
		return fmt.Errorf("timeout after %s waiting for Praefect signal for transaction %d", m.postReceiveWaitTimeout, txID)
	case <-ctx.Done():
		return ctx.Err()
	}
}
