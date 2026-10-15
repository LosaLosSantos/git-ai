package test_params_order

import (
	"context"
	"testing"
)

func testHelper1(paramA string, t *testing.T) { // want "testing.TB argument should always be passed as first parameter"
}

func testHelper2(ctx context.Context, t *testing.T) { // want "testing.TB argument should always be passed as first parameter"
}

func testHelper3(paramA string, tb testing.TB) { // want "testing.TB argument should always be passed as first parameter"
}

func testHelper4(t *testing.T, paramA string, ctx context.Context) { // want "context.Context should follow after testing.TB"
}

func testHelper5(t *testing.T) {
}

func testHelper6(t *testing.T, ctx context.Context) {
}

func testHelper7(t *testing.T, ctx context.Context, paramA string) {
}

func testHelper8(paramA string) {
}

func testHelper9(ctx context.Context, paramA string) {
}
