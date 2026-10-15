package tracing

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitializeTracerProvider tests the initialization of a tracer provider.
// Because GITLAB_TRACING parsing is handled by LabKit v2/trace, these tests
// only verify the behaviour of the wrapper itself.
func TestInitializeTracerProvider(t *testing.T) {
	tests := []struct {
		name    string
		setup   func()
		wantErr bool
	}{
		{
			name: "tracing not configured",
			setup: func() {
				_ = os.Setenv(gitlabTracingEnvVar, "")
			},
			wantErr: false,
		},
		{
			name: "otlp http tracer provider",
			setup: func() {
				_ = os.Setenv(gitlabTracingEnvVar, "otlp://localhost:4318")
			},
			wantErr: false,
		},
		{
			name: "otlps (tls) tracer provider",
			setup: func() {
				_ = os.Setenv(gitlabTracingEnvVar, "otlps://collector.example.com:4318")
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			t.Cleanup(func() { _ = os.Unsetenv(gitlabTracingEnvVar) })

			ctx := context.Background()

			closer, err := InitializeTracerProvider(ctx, "gitaly")
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, closer)
			assert.NoError(t, closer.Close())
		})
	}
}
