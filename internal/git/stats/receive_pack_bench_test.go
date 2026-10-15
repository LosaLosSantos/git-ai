package stats

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
)

func BenchmarkReceivePackParse(b *testing.B) {
	const (
		oidA = "1111111111111111111111111111111111111111"
		oidB = "2222222222222222222222222222222222222222"
	)

	benchRequest := func(refs int) []byte {
		request := pktLine(b, fmt.Sprintf("%s %s refs/heads/branch-0\x00report-status side-band-64k agent=git/2.45.0", oidA, oidB))
		for i := 1; i < refs; i++ {
			request = append(request, pktLine(b, fmt.Sprintf("%s %s refs/heads/branch-%d", oidA, oidB, i))...)
		}
		return slices.Concat(request, flush, packHeader(b, uint32(refs)))
	}

	for _, refs := range []int{1, 10, 100, 190} {
		b.Run(fmt.Sprintf("refs=%d", refs), func(b *testing.B) {
			request := benchRequest(refs)
			b.SetBytes(int64(len(request)))
			b.ResetTimer()

			for range b.N {
				var rp ReceivePack
				if err := rp.Parse(bytes.NewReader(request)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkStdinCopy(b *testing.B) {
	const (
		oidA = "1111111111111111111111111111111111111111"
		oidB = "2222222222222222222222222222222222222222"
	)

	for _, packfileSize := range []int{1 << 20, 32 << 20} {
		request := slices.Concat(
			pktLine(b, fmt.Sprintf("%s %s refs/heads/main\x00report-status side-band-64k agent=git/2.45.0", oidA, oidB)),
			flush,
			packHeader(b, 1000),
			bytes.Repeat([]byte("x"), packfileSize),
		)

		for _, withCapture := range []bool{false, true} {
			b.Run(fmt.Sprintf("packfile=%dMiB/capture=%v", packfileSize>>20, withCapture), func(b *testing.B) {
				// A real pipe as in the SSH path: a bare copy into it can use
				// splice, which the capture's MultiWriter defeats.
				pr, pw, err := os.Pipe()
				if err != nil {
					b.Fatal(err)
				}
				defer pr.Close()
				defer pw.Close()

				drained := make(chan struct{})
				go func() {
					defer close(drained)
					_, _ = io.Copy(io.Discard, pr)
				}()

				b.SetBytes(int64(len(request)))
				b.ResetTimer()

				for range b.N {
					var dst io.Writer = pw
					capture := &PrefixCapture{}
					if withCapture {
						dst = io.MultiWriter(pw, capture)
					}

					if _, err := io.Copy(dst, bytes.NewReader(request)); err != nil {
						b.Fatal(err)
					}

					if withCapture {
						var rp ReceivePack
						if err := rp.Parse(bytes.NewReader(capture.Bytes())); err != nil {
							b.Fatal(err)
						}
					}
				}

				b.StopTimer()
				pw.Close()
				<-drained
			})
		}
	}
}

func BenchmarkPrefixCaptureWrite(b *testing.B) {
	for _, size := range []int{32 * 1024, 1024 * 1024} {
		b.Run(fmt.Sprintf("chunk=%dKiB", size/1024), func(b *testing.B) {
			chunk := bytes.Repeat([]byte("x"), size)
			c := &PrefixCapture{}
			b.SetBytes(int64(size))
			b.ResetTimer()

			for range b.N {
				_, _ = c.Write(chunk)
			}
		})
	}
}
