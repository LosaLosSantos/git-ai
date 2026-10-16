package operations

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
)

type parsedPatch struct {
	authorName  string
	authorEmail string
	authorDate  time.Time
	subject     string
	body        string
	patchPath   string
}

func splitAndParseMailbox(ctx context.Context, repo *localrepo.Repo, mboxReader io.Reader, workDir string) ([]parsedPatch, error) {
	messageFiles, err := splitMailbox(ctx, repo, mboxReader, workDir)
	if err != nil {
		return nil, fmt.Errorf("splitting mailbox: %w", err)
	}

	patches := make([]parsedPatch, 0, len(messageFiles))
	for i, messageFile := range messageFiles {
		patch, err := parseMailboxMessage(ctx, repo, messageFile)
		if err != nil {
			return nil, fmt.Errorf("extracting patch %d: %w", i+1, err)
		}

		patches = append(patches, patch)
	}

	return patches, nil
}

func splitMailbox(ctx context.Context, repo *localrepo.Repo, mboxReader io.Reader, outDir string) ([]string, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "mailsplit",
			Flags: []gitcmd.Option{
				// This is a bit awkward, but we cannot use a ValueFlag
				// here because git-mailsplit(1) doesn't use parse-opts.
				gitcmd.Flag{Name: "-o" + outDir},
			},
		},
		gitcmd.WithStdin(mboxReader),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, fmt.Errorf("mailsplit: %w, stderr: %q", err, stderr.String())
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, fmt.Errorf("reading mailsplit output: %w", err)
	}

	// git-mailsplit(1) writes the messages into zero-padded, monotonically
	// increasing files like "0001", "0002". Sorting the names lexically thus
	// restores the original order.
	messageFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		messageFiles = append(messageFiles, filepath.Join(outDir, entry.Name()))
	}
	sort.Strings(messageFiles)

	return messageFiles, nil
}

func parseMailboxMessage(ctx context.Context, repo *localrepo.Repo, messagePath string) (parsedPatch, error) {
	message, err := os.Open(messagePath)
	if err != nil {
		return parsedPatch{}, fmt.Errorf("opening message: %w", err)
	}
	defer message.Close()

	bodyPath := messagePath + ".msg"
	patchPath := messagePath + ".patch"

	// git-mailinfo(1) takes an individual mbox message and splits it up into three different pieces:
	//
	//   - The commit message, which is written to the first argument.
	//   - The patch, which is written to the second argument.
	//   - The commit metadata like author and subject, which is written to stdout.
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "mailinfo",
			Args: []string{bodyPath, patchPath},
		},
		gitcmd.WithStdin(message),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return parsedPatch{}, fmt.Errorf("mailinfo: %w, stderr: %q", err, stderr.String())
	}

	patch, err := parseMailInfoHeaders(stdout.Bytes())
	if err != nil {
		return parsedPatch{}, fmt.Errorf("parsing headers: %w", err)
	}

	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return parsedPatch{}, fmt.Errorf("reading message body: %w", err)
	}

	patch.body = string(body)
	patch.patchPath = patchPath

	return patch, nil
}

// parseMailInfoHeaders parses the headers emitted by git-mailinfo(1) on stdout.
// The headers have the form "Key: value", one per line.
func parseMailInfoHeaders(headers []byte) (parsedPatch, error) {
	var result parsedPatch

	scanner := bufio.NewScanner(bytes.NewReader(headers))
	for scanner.Scan() {
		line := scanner.Text()
		// git-mailinfo(1) terminates the metadata with an empty line. Once we
		// have seen it there is nothing left to parse.
		if line == "" {
			break
		}

		key, value, found := strings.Cut(line, ":")
		if !found {
			return parsedPatch{}, fmt.Errorf("invalid mail metadata: %q", line)
		}
		value = strings.TrimSpace(value)

		switch key {
		case "Author":
			result.authorName = value
		case "Email":
			result.authorEmail = value
		case "Subject":
			result.subject = value
		case "Date":
			date, err := mail.ParseDate(value)
			if err != nil {
				return parsedPatch{}, fmt.Errorf("parsing date %q: %w", value, err)
			}
			result.authorDate = date
		}
	}

	if result.authorName == "" || result.authorEmail == "" || result.subject == "" {
		return parsedPatch{}, errors.New("mail is missing metadata")
	}

	if err := scanner.Err(); err != nil {
		return parsedPatch{}, fmt.Errorf("scanning mail headers: %w", err)
	}

	return result, nil
}
