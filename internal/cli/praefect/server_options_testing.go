//go:build gitaly_test

package praefect

import (
	"github.com/feder-cr/open_scion/internal/praefect"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
)

// defaultServerOptions are only required by our testing infrastructure. When spawning Praefect as
// an external, proxying executable we want to be able to use test-only interceptors. So if the
// binary is getting built with the `gitaly_test` tag, we set up the default server options to
// contain these interceptors and inject them in the server factory.
var defaultServerOptions = []praefect.ServerOption{
	praefect.WithUnaryInterceptor(testserver.StructErrUnaryInterceptor),
	praefect.WithStreamInterceptor(testserver.StructErrStreamInterceptor),
}
