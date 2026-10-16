package smarthttp

import (
	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/bundleuri"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
)

type server struct {
	gitalypb.UnimplementedSmartHTTPServiceServer
	logger                           log.Logger
	cfg                              config.Cfg
	locator                          storage.Locator
	packfileNegotiationMetrics       *prometheus.CounterVec
	packfileNegotiationDeepenMetrics prometheus.Histogram
	uploadPackServedBytesMetrics     *prometheus.CounterVec
	receivePackObjectsMetrics        prometheus.Histogram
	infoRefCache                     infoRefCache
	txManager                        transaction.Manager
	hookManager                      hook.Manager
	updater                          *updateref.UpdaterWithHooks
	backupLocator                    backup.Locator
	backupSink                       *backup.Sink
	localRepoFactory                 localrepo.Factory
	bundleURIManager                 *bundleuri.GenerationManager
}

// NewServer creates a new instance of a grpc SmartHTTPServer
func NewServer(deps *service.Dependencies, serverOpts ...ServerOpt) gitalypb.SmartHTTPServiceServer {
	s := &server{
		logger:      deps.GetLogger(),
		cfg:         deps.GetCfg(),
		locator:     deps.GetLocator(),
		txManager:   deps.GetTxManager(),
		hookManager: deps.GetHookManager(),
		updater:     deps.GetUpdaterWithHooks(),
		packfileNegotiationMetrics: prometheus.NewCounterVec(
			prometheus.CounterOpts{},
			[]string{"git_negotiation_feature"},
		),
		packfileNegotiationDeepenMetrics: prometheus.NewHistogram(prometheus.HistogramOpts{}),
		uploadPackServedBytesMetrics: prometheus.NewCounterVec(
			prometheus.CounterOpts{},
			[]string{"clone_type"},
		),
		receivePackObjectsMetrics: prometheus.NewHistogram(prometheus.HistogramOpts{}),
		infoRefCache:              newInfoRefCache(deps.GetLogger(), deps.GetDiskCache()),
		backupLocator:             deps.GetBackupLocator(),
		backupSink:                deps.GetBackupSink(),
		localRepoFactory:          deps.GetRepositoryFactory(),
		bundleURIManager:          deps.GetBundleURIManager(),
	}

	for _, serverOpt := range serverOpts {
		serverOpt(s)
	}

	return s
}

// ServerOpt is a self referential option for server
type ServerOpt func(s *server)

//nolint:revive // This is unintentionally missing documentation.
func WithPackfileNegotiationMetrics(c *prometheus.CounterVec) ServerOpt {
	return func(s *server) {
		s.packfileNegotiationMetrics = c
	}
}

// WithPackfileNegotiationDeepenMetrics overrides the default histogram metric.
func WithPackfileNegotiationDeepenMetrics(h prometheus.Histogram) ServerOpt {
	return func(s *server) {
		s.packfileNegotiationDeepenMetrics = h
	}
}

// WithUploadPackServedBytesMetrics overrides the default upload-pack served
// bytes counter.
func WithUploadPackServedBytesMetrics(c *prometheus.CounterVec) ServerOpt {
	return func(s *server) {
		s.uploadPackServedBytesMetrics = c
	}
}

// WithReceivePackObjectsMetrics overrides the default receive-pack objects
// histogram.
func WithReceivePackObjectsMetrics(h prometheus.Histogram) ServerOpt {
	return func(s *server) {
		s.receivePackObjectsMetrics = h
	}
}

//nolint:revive // This is unintentionally missing documentation.
func WithBundleURIManager(m *bundleuri.GenerationManager) ServerOpt {
	return func(s *server) {
		s.bundleURIManager = m
	}
}
