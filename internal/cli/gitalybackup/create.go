package gitalybackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime"
	"slices"
	"time"

	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	cli "github.com/urfave/cli/v3"
)

type serverRepository struct {
	storage.ServerInfo
	StorageName   string `json:"storage_name"`
	RelativePath  string `json:"relative_path"`
	GlProjectPath string `json:"gl_project_path"`
}

type createSubcommand struct {
	backupPath       string
	backupPathPrefix string
	parallel         int
	parallelStorage  int
	incremental      bool
	backupID         string
	serverSide       bool
}

func (cmd *createSubcommand) flags(ctx *cli.Command) {
	cmd.backupPath = ctx.String("path")
	cmd.backupPathPrefix = ctx.String("path-prefix")
	cmd.parallel = ctx.Int("parallel")
	cmd.parallelStorage = ctx.Int("parallel-storage")
	cmd.incremental = ctx.Bool("incremental")
	cmd.backupID = ctx.String("id")
	cmd.serverSide = ctx.Bool("server-side")
}

func createFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:  "path",
			Usage: "repository backup path",
		},
		&cli.StringFlag{
			Name:  "path-prefix",
			Usage: "name of the directory where the backups will be uploaded to under the backup bucket. Useful for time based naming rotation of the backup directory",
		},
		&cli.IntFlag{
			Name:  "parallel",
			Usage: "maximum number of parallel backups",
			Value: runtime.NumCPU(),
		},
		&cli.IntFlag{
			Name:  "parallel-storage",
			Usage: "maximum number of parallel backups per storage. Note: actual parallelism when combined with `-parallel` depends on the order the repositories are received.",
			Value: 2,
		},
		&cli.BoolFlag{
			Name:  "incremental",
			Usage: "creates an incremental backup if possible.",
			Value: false,
		},
		&cli.StringFlag{
			Name:  "id",
			Usage: "the backup ID used when creating a full backup.",
			Value: time.Now().UTC().Format("20060102150405"),
		},
		&cli.BoolFlag{
			Name:  "server-side",
			Usage: "use server-side backups.",
			Value: false,
		},
	}
}

func newCreateCommand() *cli.Command {
	return &cli.Command{
		Name:   "create",
		Usage:  "Create backup file",
		Action: createAction,
		Flags:  createFlags(),
	}
}

func createAction(ctx context.Context, cmd *cli.Command) error {
	logger, err := log.Configure(cmd.Writer, "json", "info")
	if err != nil {
		fmt.Printf("configuring logger failed: %v", err)
		return err
	}

	ctx, err = storage.InjectGitalyServersEnv(ctx)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	subcmd := createSubcommand{}

	subcmd.flags(cmd)

	if err := subcmd.run(ctx, logger, cmd.Reader); err != nil {
		logger.Error(err.Error())
		return err
	}
	return nil
}

func (cmd *createSubcommand) run(ctx context.Context, logger log.Logger, stdin io.Reader) error {
	// Validate backup path prefix to prevent path escapes
	if err := backup.ValidatePathPrefix(cmd.backupPathPrefix); err != nil {
		return fmt.Errorf("create: validate backup path prefix: %w", err)
	}

	pool := client.NewPool(client.WithDialOptions(client.UnaryInterceptor(), client.StreamInterceptor()))
	defer func() {
		_ = pool.Close()
	}()

	var manager backup.Operator
	if cmd.serverSide {
		if cmd.backupPath != "" {
			return fmt.Errorf("create: path cannot be used with server-side backups")
		}

		manager = backup.NewServerSideAdapter(pool)
	} else {
		sink, err := backup.ResolveSink(ctx, cmd.backupPath)
		if err != nil {
			return fmt.Errorf("create: resolve sink: %w", err)
		}

		manager = backup.NewManager(sink, logger, backup.NewLocator(sink), pool)
	}

	var opts []backup.PipelineOption
	if cmd.parallel > 0 || cmd.parallelStorage > 0 {
		opts = append(opts, backup.WithConcurrency(cmd.parallel, cmd.parallelStorage))
	}
	pipeline, err := backup.NewPipeline(logger, opts...)
	if err != nil {
		return fmt.Errorf("create pipeline: %w", err)
	}

	var latestBackupID string
	if cmd.incremental {
		latestBackupID, err = manager.ReadLatestBackupID(ctx, cmd.backupPathPrefix)
		switch {
		case errors.Is(err, backup.ErrDoesntExist):
			// No backup_ids/ markers found. Leave LatestBackupID empty so that
			// each per-repo incremental create falls back to their own latest files.
		case err != nil:
			// This is not critical due to same reason above, but here we should
			// emit a warning to the user about the failure.
			logger.WithError(err).Warn("read latest backup ID failed, will fallback to individual repository lookups")
		}
	}

	// storageServers remembers how each storage was reached, so that the backup
	// ID marker is written through the same server as the repositories.
	storageServers := make(map[string]storage.ServerInfo)

	decoder := json.NewDecoder(stdin)
	for {
		var sr serverRepository
		if err := decoder.Decode(&sr); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("create: %w", err)
		}
		if _, ok := storageServers[sr.StorageName]; !ok {
			storageServers[sr.StorageName] = sr.ServerInfo
		}
		repo := gitalypb.Repository{
			StorageName:   sr.StorageName,
			RelativePath:  sr.RelativePath,
			GlProjectPath: sr.GlProjectPath,
		}
		pipeline.Handle(ctx, backup.NewCreateCommand(manager, backup.CreateRequest{
			Server:           sr.ServerInfo,
			Repository:       &repo,
			VanityRepository: &repo,
			Incremental:      cmd.incremental,
			BackupID:         cmd.backupID,
			LatestBackupID:   latestBackupID,
			PathPrefix:       cmd.backupPathPrefix,
		}))
	}

	processedRepos, pipelineErr := pipeline.Done()

	// Write the backup ID marker even on partial failure so that repos that were
	// successfully backed up remain discoverable for restore.
	if len(processedRepos) > 0 {
		// Write the marker through a storage that a repository was backed up to.
		// Other storages in GITALY_SERVERS might not exist on their server, for
		// example a placeholder storage that points at a Praefect without a
		// virtual storage of that name. Taking the first storage in sorted order
		// keeps the choice stable across runs.
		storageName := slices.Min(slices.Collect(maps.Keys(processedRepos)))

		if err := manager.WriteBackupID(ctx, &backup.WriteBackupIDRequest{
			Server:      storageServers[storageName],
			StorageName: storageName,
			BackupID:    cmd.backupID,
			PathPrefix:  cmd.backupPathPrefix,
		}); err != nil {
			return fmt.Errorf("write backup ID: %w", err)
		}
	}

	if pipelineErr != nil {
		return fmt.Errorf("create: %w", pipelineErr)
	}

	return nil
}
