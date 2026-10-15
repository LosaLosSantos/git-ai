/*
Package praefect is a Gitaly reverse proxy for transparently routing gRPC
calls to a set of Gitaly services.
*/
package praefect

import (
	"time"

	"github.com/feder-cr/open_scion/internal/gitaly/server/auth"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/grpc/grpcstats"
	"github.com/feder-cr/open_scion/internal/grpc/listenmux"
	"github.com/feder-cr/open_scion/internal/grpc/middleware/loghandler"
	"github.com/feder-cr/open_scion/internal/grpc/middleware/panichandler"
	"github.com/feder-cr/open_scion/internal/grpc/middleware/requestinfohandler"
	"github.com/feder-cr/open_scion/internal/grpc/middleware/sentryhandler"
	"github.com/feder-cr/open_scion/internal/grpc/middleware/statushandler"
	"github.com/feder-cr/open_scion/internal/grpc/sidechannel"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/praefect/config"
	"github.com/feder-cr/open_scion/internal/praefect/datastore"
	"github.com/feder-cr/open_scion/internal/praefect/middleware"
	"github.com/feder-cr/open_scion/internal/praefect/service"
	"github.com/feder-cr/open_scion/internal/praefect/service/info"
	"github.com/feder-cr/open_scion/internal/praefect/service/server"
	"github.com/feder-cr/open_scion/internal/praefect/service/transaction"
	"github.com/feder-cr/open_scion/internal/praefect/transactions"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/tracing"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/proxy"
	grpcmwlogrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/selector"
	grpcprometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	grpccorrelation "gitlab.com/gitlab-org/labkit/correlation/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

// NewBackchannelServerFactory returns a ServerFactory that serves the RefTransactionServer on the backchannel
// connection.
func NewBackchannelServerFactory(logger log.Logger, refSvc gitalypb.RefTransactionServer, registry *sidechannel.Registry) backchannel.ServerFactory {
	logMsgProducer := loghandler.MessageProducer(
		loghandler.PropagationMessageProducer(grpcmwlogrus.DefaultMessageProducer),
		structerr.FieldsProducer,
	)

	return func() backchannel.Server {
		lm := listenmux.New(insecure.NewCredentials())
		lm.Register(sidechannel.NewServerHandshaker(registry))
		srv := grpc.NewServer(
			grpc.StatsHandler(tracing.NewGRPCServerStatsHandler()),
			grpc.ChainUnaryInterceptor(
				commonUnaryServerInterceptors(logger.WithField("component", "backchannel.PraefectServer"), logMsgProducer)...,
			),
			grpc.Creds(lm),
		)
		gitalypb.RegisterRefTransactionServer(srv, refSvc)
		grpcprometheus.Register(srv)
		return srv
	}
}

func commonUnaryServerInterceptors(logger log.Logger, messageProducer grpcmwlogrus.MessageProducer) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		grpccorrelation.UnaryServerCorrelationInterceptor(), // Must be above the metadata handler
		requestinfohandler.UnaryInterceptor,
		grpcprometheus.UnaryServerInterceptor,
		selector.UnaryServerInterceptor(logger.UnaryServerInterceptor(
			grpcmwlogrus.WithTimestampFormat(log.LogTimestampFormat),
			grpcmwlogrus.WithMessageProducer(messageProducer),
		), log.NewLogMatcher()),
		sentryhandler.UnaryLogHandler(),
		statushandler.Unary, // Should be below LogHandler
		// Panic handler should remain last so that application panics will be
		// converted to errors and logged
		panichandler.UnaryPanicHandler(logger),
	}
}

// ServerOption is an option that can be passed to `NewGRPCServer()`.
type ServerOption func(cfg *serverConfig)

type serverConfig struct {
	unaryInterceptors  []grpc.UnaryServerInterceptor
	streamInterceptors []grpc.StreamServerInterceptor
}

// WithUnaryInterceptor adds another interceptor that shall be executed for unary RPC calls.
func WithUnaryInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(cfg *serverConfig) {
		cfg.unaryInterceptors = append(cfg.unaryInterceptors, interceptor)
	}
}

// WithStreamInterceptor adds another interceptor that shall be executed for streaming RPC calls.
func WithStreamInterceptor(interceptor grpc.StreamServerInterceptor) ServerOption {
	return func(cfg *serverConfig) {
		cfg.streamInterceptors = append(cfg.streamInterceptors, interceptor)
	}
}

// NewGRPCServer returns gRPC server with registered proxy-handler and actual services praefect serves on its own.
// It includes a set of unary and stream interceptors required to add logging, authentication, etc.
func NewGRPCServer(
	deps *Dependencies,
	creds credentials.TransportCredentials,
	opts ...ServerOption,
) *grpc.Server {
	var serverCfg serverConfig
	for _, opt := range opts {
		opt(&serverCfg)
	}

	logMsgProducer := loghandler.MessageProducer(
		loghandler.PropagationMessageProducer(grpcmwlogrus.DefaultMessageProducer),
		structerr.FieldsProducer,
	)

	unaryInterceptors := append(
		commonUnaryServerInterceptors(deps.Logger.WithField("component", "praefect.UnaryServerInterceptor"), logMsgProducer),
		middleware.MethodTypeUnaryInterceptor(deps.Registry, deps.Logger),
		auth.UnaryServerInterceptor(deps.Config.Auth),
	)
	unaryInterceptors = append(unaryInterceptors, serverCfg.unaryInterceptors...)

	streamInterceptors := []grpc.StreamServerInterceptor{
		grpccorrelation.StreamServerCorrelationInterceptor(), // Must be above the metadata handler
		middleware.MethodTypeStreamInterceptor(deps.Registry, deps.Logger),
		requestinfohandler.StreamInterceptor,
		grpcprometheus.StreamServerInterceptor,
		selector.StreamServerInterceptor(deps.Logger.WithField("component", "praefect.StreamServerInterceptor").StreamServerInterceptor(
			grpcmwlogrus.WithTimestampFormat(log.LogTimestampFormat),
			grpcmwlogrus.WithMessageProducer(logMsgProducer),
		), log.NewLogMatcher()),
		sentryhandler.StreamLogHandler(),
		statushandler.Stream, // Should be below LogHandler
		auth.StreamServerInterceptor(deps.Config.Auth),
		// Panic handler should remain last so that application panics will be
		// converted to errors and logged
		panichandler.StreamPanicHandler(deps.Logger),
	}
	streamInterceptors = append(streamInterceptors, serverCfg.streamInterceptors...)

	grpcOpts := proxyRequiredOpts(deps.Director)
	grpcOpts = append(grpcOpts, []grpc.ServerOption{
		grpc.StatsHandler(tracing.NewGRPCServerStatsHandler()),
		grpc.StatsHandler(loghandler.PerRPCLogHandler{
			Underlying:     &grpcstats.PayloadBytes{},
			FieldProducers: []loghandler.FieldsProducer{grpcstats.FieldsProducer},
		}),
		grpc.ChainStreamInterceptor(streamInterceptors...),
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		// We deliberately set the server MinTime to significantly less than the client interval of 20
		// seconds to allow for network jitter. We can afford to be forgiving as the maximum number of
		// concurrent clients for a Gitaly server is typically in the hundreds and this volume of
		// keepalives won't add significant load.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time: 5 * time.Minute,
		}),
		grpc.WaitForHandlers(true),
	}...)

	// Accept backchannel connections so that we can proxy sidechannels
	// from clients (e.g. Workhorse) to a backend Gitaly server.
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	lm := listenmux.New(creds)
	lm.Register(backchannel.NewServerHandshaker(deps.Logger, backchannel.NewRegistry(), nil))
	grpcOpts = append(grpcOpts, grpc.Creds(lm))

	warnDupeAddrs(deps.Logger, deps.Config)

	srv := grpc.NewServer(grpcOpts...)
	registerServices(srv, deps.Logger, deps.TxMgr, deps.Config, deps.RepositoryStore, deps.AssignmentStore, service.Connections(deps.Conns), deps.PrimaryGetter, deps.Checks)

	if deps.Config.Failover.ElectionStrategy == config.ElectionStrategyPerRepository {
		proxy.RegisterStreamHandlers(srv, "gitaly.RepositoryService", map[string]grpc.StreamHandler{
			"RemoveRepository": RemoveRepositoryHandler(deps.RepositoryStore, deps.Logger, deps.Conns),
			"RepositoryExists": RepositoryExistsHandler(deps.RepositoryStore),
			"BackupRepository": BackupRepositoryHandler(deps.Router),
		})
		proxy.RegisterStreamHandlers(srv, "gitaly.ObjectPoolService", map[string]grpc.StreamHandler{
			"DeleteObjectPool": DeleteObjectPoolHandler(deps.RepositoryStore, deps.Logger, deps.Conns),
			"GetObjectPool":    GetObjectPoolHandler(deps.RepositoryStore, deps.Router),
		})
		proxy.RegisterStreamHandlers(srv, "gitaly.InternalGitaly", map[string]grpc.StreamHandler{
			"WalkRepos": WalkReposHandler(deps.RepositoryStore),
		})
	}

	return srv
}

func proxyRequiredOpts(director proxy.StreamDirector) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ForceServerCodec(proxy.NewCodec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
	}
}

// registerServices registers services praefect needs to handle RPCs on its own.
func registerServices(
	srv *grpc.Server,
	logger log.Logger,
	tm *transactions.Manager,
	conf config.Config,
	rs datastore.RepositoryStore,
	assignmentStore AssignmentStore,
	conns service.Connections,
	primaryGetter info.PrimaryGetter,
	checks []service.CheckFunc,
) {
	// ServerServiceServer is necessary for the ServerInfo RPC
	gitalypb.RegisterServerServiceServer(srv, server.NewServer(conf, logger, conns, checks))
	gitalypb.RegisterPraefectInfoServiceServer(srv, info.NewServer(conf, logger, rs, assignmentStore, conns, primaryGetter))
	gitalypb.RegisterRefTransactionServer(srv, transaction.NewServer(tm))
	healthpb.RegisterHealthServer(srv, auth.UnauthenticatedHealthService{HealthServer: health.NewServer()})
	reflection.Register(srv)
	grpcprometheus.Register(srv)
}

func warnDupeAddrs(logger log.Logger, conf config.Config) {
	var fishy bool

	for _, virtualStorage := range conf.VirtualStorages {
		addrSet := map[string]struct{}{}
		for _, n := range virtualStorage.Nodes {
			_, ok := addrSet[n.Address]
			if ok {
				logger.WithField("conflicting_address", n.Address).Warn("more than one backend node is hosted at same address")
				fishy = true
				continue
			}
			addrSet[n.Address] = struct{}{}
		}
		if fishy {
			logger.Warn("your Praefect configuration may not offer actual redundancy")
		}
	}
}
