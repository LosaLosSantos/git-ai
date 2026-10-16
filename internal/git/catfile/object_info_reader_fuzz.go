//go:build gofuzz

package catfile

import (
	"bufio"
	"bytes"

	"github.com/feder-cr/git-ai/internal/git"
)

func Fuzz(data []byte) int {
	reader := bufio.NewReader(bytes.NewReader(data))
	ParseObjectInfo(git.ObjectHashSHA1, reader)
	return 0
}
