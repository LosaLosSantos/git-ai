package dontpanic_test

import (
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/dontpanic"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestTry(t *testing.T) {
	dontpanic.Try(testhelper.SharedLogger(t), func() { panic("don't panic") })
}

func TestTryNoPanic(t *testing.T) {
	invoked := false
	dontpanic.Try(testhelper.SharedLogger(t), func() { invoked = true })
	require.True(t, invoked)
}

func TestGo(t *testing.T) {
	done := make(chan struct{})
	dontpanic.Go(testhelper.SharedLogger(t), func() {
		defer close(done)
		panic("don't panic")
	})
	<-done
}

func TestGoNoPanic(t *testing.T) {
	done := make(chan struct{})
	dontpanic.Go(testhelper.SharedLogger(t), func() { close(done) })
	<-done
}

func TestGoForever(t *testing.T) {
	var i int
	recoveredQ := make(chan struct{})
	expectPanics := 5

	fn := func() {
		defer func() { recoveredQ <- struct{}{} }()
		i++

		if i > expectPanics {
			close(recoveredQ)
		}

		panic("don't panic")
	}

	forever := dontpanic.NewForever(testhelper.SharedLogger(t), time.Microsecond)
	forever.Go(fn)

	var actualPanics int
	for range recoveredQ {
		actualPanics++
	}
	require.Equal(t, expectPanics, actualPanics)
	forever.Cancel()
}
