package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/oasisprotocol/oasis-core/go/common"
	cmnGrpc "github.com/oasisprotocol/oasis-core/go/common/grpc"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-sdk/client-sdk/go/client"
	"github.com/oasisprotocol/oasis-sdk/client-sdk/go/modules/core"
	"github.com/spf13/cobra"
	"github.com/uptrace/bun"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/oasisprotocol/emerald-web3-gateway/archive"
	"github.com/oasisprotocol/emerald-web3-gateway/conf"
	"github.com/oasisprotocol/emerald-web3-gateway/db/migrations"
	"github.com/oasisprotocol/emerald-web3-gateway/filters"
	"github.com/oasisprotocol/emerald-web3-gateway/gas"
	"github.com/oasisprotocol/emerald-web3-gateway/indexer"
	"github.com/oasisprotocol/emerald-web3-gateway/log"
	"github.com/oasisprotocol/emerald-web3-gateway/rpc"
	"github.com/oasisprotocol/emerald-web3-gateway/server"
	"github.com/oasisprotocol/emerald-web3-gateway/storage"
	"github.com/oasisprotocol/emerald-web3-gateway/storage/psql"
	"github.com/oasisprotocol/emerald-web3-gateway/version"
)

var (
	// Path to the configuration file.
	configFile string
	// Allow unsafe operations.
	allowUnsafe bool

	// Oasis-web3-gateway root command.
	rootCmd = &cobra.Command{
		Use:     "emerald-web3-gateway",
		Short:   "emerald-web3-gateway",
		Version: version.Software,
		RunE:    exec,
	}

	// Truncate DB.
	truncateCmd = &cobra.Command{
		Use:   "truncate-db",
		Short: "truncate-db",
		RunE:  truncateExec,
	}

	// Migrate DB.
	migrateCmd = &cobra.Command{
		Use:   "migrate-db",
		Short: "migrate-db",
		RunE:  migrateExec,
	}
)

func initVersions() {
	cobra.AddTemplateFunc("toolchain", func() interface{} { return version.Toolchain })
	cobra.AddTemplateFunc("sdk", func() interface{} { return version.GetOasisSDKVersion() })

	rootCmd.SetVersionTemplate(`Software version: {{.Version}}
Oasis SDK version: {{ sdk }}
Go toolchain version: {{ toolchain }}
`)
}

func init() {
	initVersions()

	rootCmd.Flags().StringVar(&configFile, "config", "./conf/server.yml", "path to the config.yml file")

	truncateCmd.Flags().StringVar(&configFile, "config", "./conf/server.yml", "path to the config.yml file")
	truncateCmd.Flags().BoolVar(&allowUnsafe, "unsafe", false, "allow destructive unsafe operations")
	rootCmd.AddCommand(truncateCmd)

	migrateCmd.Flags().StringVar(&configFile, "config", "./conf/server.yml", "path to the config.yml file")
	rootCmd.AddCommand(migrateCmd)
}

func main() {
	_ = rootCmd.Execute()
}

func exec(cmd *cobra.Command, args []string) error {
	if err := runRoot(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	return nil
}

func truncateExec(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	// Initialize server config
	cfg, err := conf.InitConfig(configFile)
	if err != nil {
		return err
	}
	// Initialize logging.
	if err = log.InitLogging(cfg); err != nil {
		return err
	}
	logger := logging.GetLogger("truncate-db")

	if !allowUnsafe {
		logger.Error("unsafe commands not allowed, ensure `--unsafe` is set")
		return fmt.Errorf("unsafe not allowed")
	}

	// Initialize db.
	db, err := psql.InitDB(ctx, cfg.Database, true, false)
	if err != nil {
		logger.Error("failed to initialize db", "err", err)
		return err
	}

	// Truncate database.
	if err := migrations.DropTables(ctx, db.DB.(*bun.DB)); err != nil {
		logger.Error("failed to truncate db", "err", err)
		return err
	}

	logger.Info("database truncated")

	return nil
}

func migrateExec(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	// Initialize server config
	cfg, err := conf.InitConfig(configFile)
	if err != nil {
		return err
	}
	// Initialize logging.
	if err = log.InitLogging(cfg); err != nil {
		return err
	}
	logger := logging.GetLogger("migrate-db")

	// Initialize db.
	db, err := psql.InitDB(ctx, cfg.Database, true, false)
	if err != nil {
		logger.Error("failed to initialize db", "err", err)
		return err
	}

	// Run migrations.
	if err = db.RunMigrations(ctx); err != nil {
		return fmt.Errorf("failed to migrate DB: %w", err)
	}

	return nil
}

func runRoot() error {
	ctx := context.Background()
	// Initialize server config
	cfg, err := conf.InitConfig(configFile)
	if err != nil {
		return err
	}
	// Initialize logging.
	if err = log.InitLogging(cfg); err != nil {
		return err
	}

	logger := logging.GetLogger("main")

	// Decode hex runtime ID into something we can use.
	var runtimeID common.Namespace
	if err = runtimeID.UnmarshalHex(cfg.RuntimeID); err != nil {
		logger.Error("malformed runtime ID", "err", err)
		return err
	}

	// Establish a gRPC connection with the client node.
	logger.Info("connecting to local node", "addr", cfg.NodeAddress)
	conn, err := cmnGrpc.Dial(cfg.NodeAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("failed to establish connection", "err", err)
		return err
	}
	defer conn.Close()

	// Create the runtime client with account module query helpers.
	rc := client.New(conn, runtimeID)

	// For now, "disable" write access to the DB in a kind of kludgy way
	// if the indexer is disabled.  Yes this means that no migrations
	// can be done.  Deal with it.
	dbReadOnly := cfg.IndexingDisable || cfg.IndexingSQLFollow

	// Initialize db for migrations (higher timeouts).
	db, err := psql.InitDB(ctx, cfg.Database, true, dbReadOnly)
	if err != nil {
		logger.Error("failed to initialize db", "err", err)
		return err
	}
	// Run migrations.
	if err = db.RunMigrations(ctx); err != nil {
		return fmt.Errorf("failed to migrate DB: %w", err)
	}
	if err = db.DB.(*bun.DB).DB.Close(); err != nil {
		return fmt.Errorf("failed to close migrations DB: %w", err)
	}

	// Initialize db again, now with configured timeouts.
	var storage storage.Storage
	storage, err = psql.InitDB(ctx, cfg.Database, false, dbReadOnly)
	if err != nil {
		logger.Error("failed to initialize db", "err", err)
		return err
	}
	// Monitoring if enabled.
	if cfg.Gateway.Monitoring.Enabled() {
		storage = psql.NewMetricsWrapper(storage)
	}

	// Create Indexer.
	subBackend, err := filters.NewSubscribeBackend(storage)
	if err != nil {
		return err
	}
	backend := indexer.NewIndexBackend(runtimeID, storage, subBackend)
	indx, backend, err := indexer.New(ctx, backend, rc, runtimeID, storage, cfg)
	if err != nil {
		logger.Error("failed to create indexer", err)
		return err
	}
	indx.Start()

	// Create event system.
	es := filters.NewEventSystem(subBackend)

	// Create web3 gateway instance.
	w3, err := server.New(ctx, cfg.Gateway)
	if err != nil {
		logger.Error("failed to create web3", err)
		return err
	}
	gasPriceOracle := gas.New(ctx, backend, core.NewV1(rc))
	if err = gasPriceOracle.Start(); err != nil {
		logger.Error("failed to start gas price oracle", err)
		return err
	}

	var archiveClient *archive.Client
	if cfg.ArchiveURI != "" {
		if archiveClient, err = archive.New(ctx, cfg.ArchiveURI, cfg.ArchiveHeightMax); err != nil {
			logger.Error("failed to create archive client", err)
			return err
		}
	}

	w3.RegisterAPIs(rpc.GetRPCAPIs(ctx, rc, archiveClient, backend, gasPriceOracle, cfg.Gateway, es))
	w3.RegisterHealthChecks([]server.HealthCheck{indx})

	svr := server.Server{
		Config: cfg,
		Web3:   w3,
		DB:     storage,
	}

	if err = svr.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Unable to start Web3 server: %v\n", err)
		return err
	}

	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigc)

		<-sigc
		logger.Info("Got interrupt, shutting down...")
		go svr.Close()
		go indx.Stop()
		go gasPriceOracle.Stop()
	}()

	svr.Wait()

	return nil
}
