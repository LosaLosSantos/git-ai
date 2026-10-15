package signature

import (
	"context"
	"strings"
	"testing"
)

// ExtractSignature extracts the signature from a commit object for testing
// purposes
func ExtractSignature(tb testing.TB, ctx context.Context, objectData []byte) (string, string) {
	const gpgSignaturePrefix = "gpgsig "
	const gpgSignaturePrefixSha256 = "gpgsig-sha256 "

	var (
		gpgsig, dataWithoutGpgSig string
		inSignature               bool
	)

	lines := strings.Split(string(objectData), "\n")

	for i, line := range lines {
		if line == "" {
			dataWithoutGpgSig += "\n" + strings.Join(lines[i+1:], "\n")
			break
		}

		if strings.HasPrefix(line, gpgSignaturePrefix) {
			inSignature = true
			gpgsig += strings.TrimPrefix(line, gpgSignaturePrefix)
		} else if strings.HasPrefix(line, gpgSignaturePrefixSha256) {
			inSignature = true
			gpgsig += strings.TrimPrefix(line, gpgSignaturePrefixSha256)
		} else if inSignature {
			gpgsig += "\n" + strings.TrimPrefix(line, " ")
		} else {
			dataWithoutGpgSig += line + "\n"
		}
	}

	return gpgsig, dataWithoutGpgSig
}
