package setup

import (
	"github.com/feder-cr/open_scion/internal/gitaly/server/auth"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/service/analysis"
	gitalybackup "github.com/feder-cr/open_scion/internal/gitaly/service/backup"
	"github.com/feder-cr/open_scion/internal/gitaly/service/blob"
	"github.com/feder-cr/open_scion/internal/gitaly/service/changeset"
	"github.com/feder-cr/open_scion/internal/gitaly/service/cleanup"
	"github.com/feder-cr/open_scion/internal/gitaly/service/commit"
	"github.com/feder-cr/open_scion/internal/gitaly/service/conflicts"
	"github.com/feder-cr/open_scion/internal/gitaly/service/derived"
	"github.com/feder-cr/open_scion/internal/gitaly/service/diff"
	"github.com/feder-cr/open_scion/internal/gitaly/service/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service/internalgitaly"
	"github.com/feder-cr/open_scion/internal/gitaly/service/object"
	"github.com/feder-cr/open_scion/internal/gitaly/service/objectpool"
	"github.com/feder-cr/open_scion/internal/gitaly/service/operations"
	"github.com/feder-cr/open_scion/internal/gitaly/service/partition"
	"github.com/feder-cr/open_scion/internal/gitaly/service/ref"
	"github.com/feder-cr/open_scion/internal/gitaly/service/remote"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/gitaly/service/review"
	"github.com/feder-cr/open_scion/internal/gitaly/service/search"
	"github.com/feder-cr/open_scion/internal/gitaly/service/server"
	"github.com/feder-cr/open_scion/internal/gitaly/service/smarthttp"
	"github.com/feder-cr/open_scion/internal/gitaly/service/ssh"
	"github.com/feder-cr/open_scion/internal/gitaly/service/storagecore"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	grpcprometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var (
	smarthttpPackfileNegotiationMetrics = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "gitaly",
			Subsystem: "smarthttp",
			Name:      "packfile_negotiation_requests_total",
			Help:      "Total number of features used for packfile negotiations",
		},
		[]string{"git_negotiation_feature"},
	)

	sshPackfileNegotiationMetrics = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "gitaly",
			Subsystem: "ssh",
			Name:      "packfile_negotiation_requests_total",
			Help:      "Total number of features used for packfile negotiations",
		},
		[]string{"git_negotiation_feature"},
	)

	smarthttpPackfileNegotiationDeepenMetrics = newPackfileNegotiationDeepenHistogram("smarthttp")
	sshPackfileNegotiationDeepenMetrics       = newPackfileNegotiationDeepenHistogram("ssh")

	smarthttpUploadPackServedBytes = newUploadPackServedBytesMetric("smarthttp")
	sshUploadPackServedBytes       = newUploadPackServedBytesMetric("ssh")

	smarthttpReceivePackObjects = newReceivePackObjectsHistogram("smarthttp")
	sshReceivePackObjects       = newReceivePackObjectsHistogram("ssh")
)

// newReceivePackObjectsHistogram returns a histogram of the number of objects
// in a packfile received during receive-pack.
func newReceivePackObjectsHistogram(subsystem string) prometheus.Histogram {
	return promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "gitaly",
			Subsystem: subsystem,
			Name:      "receive_pack_objects",
			Help:      "Distribution of the number of objects in a packfile received during receive-pack",
			Buckets:   prometheus.ExponentialBucketsRange(1, 10_000_000, 10),
		},
	)
}

// newUploadPackServedBytesMetric returns a counter of the number of bytes
// served to clients during upload-pack, labeled by the type of clone. Note
// that the byte counts are not directly comparable across transports because
// the smarthttp and ssh code paths frame the response differently.
func newUploadPackServedBytesMetric(subsystem string) *prometheus.CounterVec {
	return promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "gitaly",
			Subsystem: subsystem,
			Name:      "upload_pack_served_bytes_total",
			Help:      "Total number of bytes served to clients during upload-pack, by clone type",
		},
		[]string{"clone_type"},
	)
}

func newPackfileNegotiationDeepenHistogram(subsystem string) prometheus.Histogram {
	return promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "gitaly",
			Subsystem: subsystem,
			Name:      "packfile_negotiation_deepen",
			Help:      "Distribution of deepen values requested during packfile negotiation",
			// These buckets help us capture the extreme ends of the "deepen" value (1 and INFINITE_DEPTH),
			// as well as a few interesting ranges in between.
			Buckets: append(
				[]float64{1, 2, 5, 10},
				append(
					// Upper-bound this to the value immediately preceding INFINITE_DEPTH.
					// The count of buckets is pretty arbitrary.
					prometheus.ExponentialBucketsRange(20, float64(0x7ffffffe), 4),
					float64(0x7fffffff),
				)...,
			),
		},
	)
}

// RegisterAll will register all the known gRPC services on  the provided gRPC service instance.
func RegisterAll(srv *grpc.Server, deps *service.Dependencies) {
	gitalypb.RegisterAnalysisServiceServer(srv, analysis.NewServer(deps))
	gitalypb.RegisterBackupServiceServer(srv, gitalybackup.NewServer(deps))
	gitalypb.RegisterBlobServiceServer(srv, blob.NewServer(deps))
	gitalypb.RegisterCleanupServiceServer(srv, cleanup.NewServer(deps))
	gitalypb.RegisterCommitServiceServer(srv, commit.NewServer(deps))
	gitalypb.RegisterDiffServiceServer(srv, diff.NewServer(deps))
	gitalypb.RegisterOperationServiceServer(srv, operations.NewServer(deps))
	gitalypb.RegisterRefServiceServer(srv, ref.NewServer(deps))
	gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	gitalypb.RegisterSSHServiceServer(srv, ssh.NewServer(deps,
		ssh.WithPackfileNegotiationMetrics(sshPackfileNegotiationMetrics),
		ssh.WithPackfileNegotiationDeepenMetrics(sshPackfileNegotiationDeepenMetrics),
		ssh.WithUploadPackServedBytesMetrics(sshUploadPackServedBytes),
		ssh.WithReceivePackObjectsMetrics(sshReceivePackObjects),
	))
	gitalypb.RegisterSmartHTTPServiceServer(srv, smarthttp.NewServer(deps,
		smarthttp.WithPackfileNegotiationMetrics(smarthttpPackfileNegotiationMetrics),
		smarthttp.WithPackfileNegotiationDeepenMetrics(smarthttpPackfileNegotiationDeepenMetrics),
		smarthttp.WithUploadPackServedBytesMetrics(smarthttpUploadPackServedBytes),
		smarthttp.WithReceivePackObjectsMetrics(smarthttpReceivePackObjects),
	))
	gitalypb.RegisterConflictsServiceServer(srv, conflicts.NewServer(deps))
	gitalypb.RegisterRemoteServiceServer(srv, remote.NewServer(deps))
	gitalypb.RegisterServerServiceServer(srv, server.NewServer(deps))
	gitalypb.RegisterObjectPoolServiceServer(srv, objectpool.NewServer(deps))
	gitalypb.RegisterHookServiceServer(srv, hook.NewServer(deps))
	gitalypb.RegisterInternalGitalyServer(srv, internalgitaly.NewServer(deps))
	gitalypb.RegisterPartitionServiceServer(srv, partition.NewServer(deps))

	// Graft extensions (see doc/graft/PLAN.md).
	gitalypb.RegisterObjectServiceServer(srv, object.NewServer(deps))
	gitalypb.RegisterChangesetServiceServer(srv, changeset.NewServer(deps))
	gitalypb.RegisterDerivedDataServiceServer(srv, derived.NewServer(deps))
	gitalypb.RegisterSearchServiceServer(srv, search.NewServer(deps))
	gitalypb.RegisterReviewServiceServer(srv, review.NewServer(deps))
	gitalypb.RegisterStorageCoreServiceServer(srv, storagecore.NewServer(deps))

	healthpb.RegisterHealthServer(srv, auth.UnauthenticatedHealthService{HealthServer: health.NewServer()})
	reflection.Register(srv)
	grpcprometheus.Register(srv)
}
